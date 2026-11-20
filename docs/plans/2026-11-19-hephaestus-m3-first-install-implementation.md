# hephaestus M3 First Talos Install Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Spec:** docs/designs/2026-11-18-hephaestus-m3-first-install-design.md (approved by the owner 2026-11-19, revision 6; its §11 lists findings this plan resolves in "Rulings made while planning").

**Goal:** Approve a discovered machine into a host of a git-imported Topf cluster and have it install Talos unattended: secrets from 1Password sealed in the database, a sandboxed render, prefetched boot assets, a single-use ticket and `/config` over TLS, all driven by an admin CLI.

**Architecture:** New packages `clusters` (definitions, git, pairs, approval, drift, the `/config` render), `render` (load rules, template allow-list, redaction, hashes over the vendored Topf path), `secrets` (`op://` resolver over the 1Password SDK, request counter), `factory` (image-factory client) and the vendored `third_party/topf` and `third_party/imagefactory`. `fleet` gains bindings, tickets, the `talos_cache` repository and transaction-scoped exports; `toolcache` gains a fetch-source seam with a Talos source; `store` makes every tagged blob durable; `boot` feeds rows 8–10, mints tickets, serves the install script, `/t/` and `/config`; `cmd/hephaestus` wires `serve` and adds the `cluster`, `machine`, `audit` and `unknown-action` commands. Dependency direction: `cmd → boot, clusters, toolcache`; `boot → clusters, fleet, store`; `clusters → render, secrets, fleet, datakey`; `render → third_party/*`; `toolcache → fleet, store, factory`; `fleet` imports none of the new packages.

**Tech Stack:** Go 1.26 language level, toolchain go1.27.4; bun, goose, modernc SQLite, pgx; Cobra/viper (cmd only); OpenTelemetry; oras-go v3.0.0; new: go-git v5.19.3, onepassword-sdk-go v0.4.1 (extism v1.7.1, wazero v1.11.0), Talos machinery v1.14.4, sprig v3.3.0, go.yaml.in/yaml/v4 (Topf's pin); Docker (colima) for Postgres tests and the parity container.

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

**A/B files:** 1 (Task 15: `internal/toolcache/fetcher.go`).

## Rulings made while planning

Each line: the ruling, the reason, the cost if wrong. The spec's §11 items are resolved here; the tasks carry them.

- Ruling (§11.1): `/config` recording logs and counts its first failure at once (`hephaestus.config.record.failures`), runs one background retry loop per ticket id (a map keyed by ticket id in the `boot` handlers), stops silently when the ticket is found consumed or the machine is `installing` on a newer ticket, and the recording transaction revokes any other live ticket of the machine. Recordings still pending at shutdown are logged (`config recording abandoned`) with the machine id. The loop runs under the `work` stage context. Reason: spec §11.1. Cost: none beyond a goroutine per rare failure.
- Ruling (§11.2): the "recorded but not received" runbook has two branches: `installing` → `machine abort` then reboot; `install-unconfirmed` → `topf reset`, `machine reinstall --topf-reset-full-done`, reboot. Both are in the `config:pending` check output and `docs/`. Cost: none.
- Ruling (§11.3): git clones are full (no depth); a move-aside raises the alert `git.store_moved_aside` (audit + `hephaestus.alerts{kind="git_store_moved_aside"}`); at most one move-aside per hour (a timestamp on the git store; a second corrupt read within the hour is reported and skipped); after a re-clone the tree cache is refilled for every approved and pending pair before the next poll; the corruption predicate is `gitCorrupt(err) bool`, true for `plumbing.ErrObjectNotFound` and for the packfile/idx decode sentinels Task 11 lists by running `go doc`. Cost: a missed sentinel leaves a corrupt store reported rather than repaired.
- Ruling (§11.4): `render:approved` is keyed on each cluster's current approved pair id: approving a new pair clears it; a cache miss sets neither drift nor `render:approved` (the §5.3 rule wins; §5.6's list loses "missing from the cache"). Cost: none.
- Ruling (§11.5): every non-empty resolved value is redacted whole, whatever its length; the 8-character and PEM-armour skips apply only to the individual lines of a multi-line value; the fail-closed check uses the same skip set; the marker is the plain word `REDACTED`; a redacted render that does not parse as YAML refuses the pair. Cost: a 1–7 character secret also blanks equal substrings elsewhere in the display render (display only).
- Ruling (§11.6): the SDK smoke test is `TestOnePassword_SmokeDummyToken` in `internal/secrets`, build tag `sdksmoke`, run by a new `go:sdk-smoke` task inside `go:ci` with `CGO_ENABLED=0`: it builds the real client with token `ops_hephaestus-smoke-test` and requires `ResolveAll` to return an error (any class) within 60 s and no panic. It proves the WASM core loads cgo-free; whether creation touches the network is then measured, not assumed. Cost: needs network in `task ci`; an offline CI fails this one test.
- Ruling (§11.7): Task 5's first step is the parity spike: build Topf from a git checkout of `70cdbc83cfbadc023ce4c7c0e3180eedfa577f16` in a pinned `golang:1.27.4` container (module downloads allowed) and assert `go version -m` shows machinery v1.14.4. If that build fails for a reason other than the network, `PLAN: BLOCKED — every path is a guess: parity reference build`. Cost: none.
- Ruling (§11.8, minor set): the command-line bound is 2046 bytes on every platform (iPXE BIOS `bzimage.c:314`); the denied sprig list names `mustDateModify` and `must_date_modify` (there is no `dateModify`); embedded references are detected with the unanchored `ref\+[A-Za-z0-9]+://` and the accepted grammar excludes space and `+` inside the reference; row 8's create is a `clusters` transaction (`EnsureAssets`), so `boot` calls `clusters`, never `fleet`, for it; failed-tree and whole-call error text is stored sealed (column `*_sealed`), never masked; `kernel command line too long` is an alert; the format byte is `0x01`, prefixed to the sealed bytes and also bound into the additional data inside `datakey`; the canary is the settings row `data_key_canary`; dummy schematic values are `x` repeated to the real value's length (`y` when the value is all `x`), and a dummy-render error refuses the tree; upper-case host names are refused (not lowercased); a reference in `nodes[].host`, `nodes[].ip`, `schematicId` or `talosVersion` is refused; a wrong-hash present blob is replaced by writing the new blob to `ingest/` and renaming over it. Cost: none.
- Ruling (spec silent): the fetch claim order is utility rows at priority 1, then Talos rows wanted by an `assigned` machine, then utility rows at priority 0, then other Talos rows; within a step oldest `updated_at` first. Reason: the spec's three classes need a deterministic cross-table order. Cost: a starved low-class row waits behind higher classes, as today.
- Ruling (spec silent): the clusters, pairs, versions and renders repository lives in `internal/clusters/repository.go`, so `.taskfiles/go.yml` `DB_PKGS` widens to `./internal/db/... ./internal/fleet/... ./internal/clusters/...`. Cost: none.
- Ruling (spec silent): the vendored Topf code is flattened into `internal/third_party/topf/{config,render,schematic}` because Go `internal/` visibility forbids importing Topf's `internal/topf` from `internal/render`; each copied file keeps its header and carries `// hephaestus: changed — <what>` where changed. Cost: none.
- Ruling (spec silent): the factory base URL lives alone in `internal/factory/base.go` (`const Base = "https://factory.talos.dev"`) so the e2e overlay replaces exactly that file. Cost: none.
- Ruling (spec silent): the e2e smart-HTTP git server and the fake factory are one Go program, `test/e2e/upstreams/main.go`, run in a `golang:1.27.4` container (which carries `git` for `git http-backend` through `net/http/cgi`) on the e2e network. Cost: none.

## Global Constraints

Every task's requirements include these.

- Module `go.jacaudi.dev/hephaestus`; `go 1.26.0` and `toolchain go1.27.4` are never raised. If `go mod tidy` raises them, `PLAN: BLOCKED — dependency raises the go line`.
- Builds use `CGO_ENABLED=0` (`go:build`, `go:cross`, the image). The 1Password SDK must build without cgo.
- Standards: `~/Projects/github/go-tricorder/docs/standards/go-standards.md` and the repo's `.golangci.yml` (golangci-lint v2.14.0). revive `argument-limit: 5`: functions needing more take a request struct. Cross-package composite literals are keyed.
- TLS: every `tls.Config` has `MinVersion: tls.VersionTLS13`; never `InsecureSkipVerify`. Owner: "1.3 is a must".
- No `init()`; no package-level variables except `ErrX` sentinels, `//go:embed` vars and `cmd/` (otherwise `//nolint:gochecknoglobals // <reason>`).
- viper and cobra only in `cmd/hephaestus`. `reflect` is banned in non-test library code except where Task 4 needs it in a `_test.go` file.
- Every exported symbol has a doc comment; each new package has `doc.go`.
- Logging: `log/slog`, snake_case keys, error key `error`; library code never logs at Info or above (it reports through an error handler, as `boot` does); never log a query string, the database URL, a ticket, a token, a reference's value or third-party error text from resolving or rendering.
- HTTP clients: never `http.DefaultClient`/`http.Get` in hephaestus's own code (forbidigo); upstream fetches use `toolcache.NewHTTPClient`. The one deliberate exception is Task 20's mutation of `http.DefaultTransport` and `http.DefaultClient.Transport`, with `//nolint:forbidigo // <reason>`.
- Errors: wrap with `%w`; sentinels `ErrX`; error types end in `Error`; tests use `errors.Is` and `errors.AsType`.
- Times: UTC; functions take `at time.Time`; stored values are truncated to microseconds (`stamp`); tests use `t0 := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)` and compare with `Equal`.
- Databases: SQLite and Postgres both. One suite function per repository, run as `TestX_SQLite` (untagged) and `TestX_Postgres` (`//go:build integration`, `*_integration_test.go`), with a tagged `TestMain` calling `dbtest.RunWithPostgres(m)` in every package that has Postgres tests. `cover:db` prints no `not executed:` line. No transaction is held across a network call, a git read or a render (SQLite has one connection).
- Lock order in every transaction: `clusters`, `cluster_pairs`, `secret_versions`, `host_renders`, `machines`, `tickets`, `talos_cache`, `utility_cache` (Postgres: a `SELECT … FOR UPDATE` or the repo's no-op-update idiom on the `clusters` row first). Conditional updates re-check what was read; a 0-row match or a deadlock/serialisation error retries once from the start.
- Goroutine tests end with `defer goleak.VerifyNone(t)`; HTTP test clients set `DisableKeepAlives`.
- Fuzz every byte parser (seeded with its table inputs); the owning task runs it once with `-fuzztime=30s`.
- Command lines: every `Run:` line is prefixed `rtk proxy`; pipelines run as `rtk proxy sh -c "…"`. Anything needing docker runs with `DOCKER_HOST=unix://$HOME/.colima/default/docker.sock`. macOS host: no `timeout` command; colima shares only `$HOME`, so never bind-mount `/tmp`; scratch goes under the gitignored `build/`. A docker failure is `PLAN: BLOCKED — environment: docker unreachable at DOCKER_HOST`.
- Nothing is installed on the host. `git` on the host is used by the git integration tests (it is present); its absence fails the test, never skips it.
- RED steps: a test that cannot compile because the symbol does not exist yet is the expected failure; never add a stub to reach a different failure.
- Commits: `rtk proxy git add <explicit paths>` then `rtk proxy git -c commit.gpgsign=false commit -m "<type>: <subject>" -m "Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>"` (fix round 2 on opus: `Claude Opus 5.5`). No `claude.ai/code/session_` URL anywhere. No comment, test name or doc mentions a review, finding or attack.
- Gate per task: `DOCKER_HOST=unix://$HOME/.colima/default/docker.sock rtk proxy task ci` exits 0 with `0 issues.`, `No vulnerabilities found.` and no `not executed:` line.

**Dependencies** (`rtk proxy go get <module>@<version>` in the first task that imports it, then `rtk proxy go mod tidy`):

| Module | Version | First task |
|---|---|---|
| github.com/siderolabs/talos/pkg/machinery | v1.14.4 | 1 |
| github.com/Masterminds/sprig/v3 | v3.3.0 | 1 |
| go.yaml.in/yaml/v4 | the version Topf `70cdbc83…` requires (read from its go.mod in Task 1 Step 1) | 1 |
| github.com/1password/onepassword-sdk-go | v0.4.1 | 6 |
| github.com/go-git/go-git/v5 | v5.19.3 | 11 |

Any other module Topf's render path requires is added at the version Topf's go.mod at the full SHA names, and listed in the task report.

**Fixtures:** FQDN `hephaestus.example.test`; subnet `10.10.20.0/24`; address `10.10.20.5`; client `10.10.20.50`; UUID `4c4c4544-0042-3510-8052-b4c04f4e4d32`; MACs `aa:bb:cc:00:00:01` upward; cluster `lab` with hosts `cp-01` (controlplane, `10.10.20.11`), `wrk-01` (worker, `10.10.20.21`); Talos `v1.12.11`, Kubernetes `v1.35.10`; plain schematic id `376567988ad370138ad8b2698212367b8edcb69b5fd68c80be1f2ec7d603b4ba`; test data key `base64(32 × 0x01)` = `AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE=`.

## Review Focus

The input classes and failure modes most likely to bite, each pinned to the test that covers it.

1. **A config served for a pair the admin did not approve.** An approval landing while `/config` renders, or a render reading another pair's tree. Pinned: Task 13 `TestApproveCluster_*/ApprovalDuringConfigSetsNoDrift`; Task 19 `TestConfig_RendersTicketPair`, `TestConfig_ApprovalRaceRecordsOlderPair`.
2. **A secret value reaching a display render, a log, the database in clear, or the public factory.** Multi-line, base64-encoded, YAML-quoted values and schematic `.tpl` bodies. Pinned: Task 4 `TestRedact_*`, `TestSchematicBody_TransformedSecretRefused`; Task 12 `TestService_FailedTreeErrorIsSealed`.
3. **A machine stuck with no command to free it.** Expired unconsumed tickets, broken Talos rows, delivered-but-unrecorded configs, an interrupted tree. Pinned: Task 8 `TestTickets_*/ExpiredLiveTicketRevokedByMint`; Task 9 `TestTalosCache_*/ResetHashFromReady`; Task 12 `TestService_InterruptedTreeNotReprocessed`; Task 19 `TestConfig_RecordingRetryStops*`.
4. **Two machines claiming one host, or a clone getting an install.** Concurrent approves, rebind of a junk machine, a junk mark after binding. Pinned: Task 7 `TestBinding_*/ConcurrentApproveOneHost`, `RebindRefusesJunk`; Task 8 `TestTickets_*/JunkMarkRevokes`; Task 19 `TestConfig_JunkRefused`.
5. **Resume splicing two upstream versions, or an unpinned first fetch trusting a short body.** Pinned: Task 16 `TestFetcher_206WithOtherETagRestarts`; Task 17 `TestTalosSource_FirstFetchSizedFromContentRange`, `TestTalosSource_RecordedHashMismatchIsIntegrity`.

---

### Task 1: Vendor the Topf render path and the image-factory schematic ID
Kind: rewrite

**Files:**
- Create: `internal/third_party/topf/config/`, `internal/third_party/topf/render/`, `internal/third_party/topf/schematic/`, `internal/third_party/topf/LICENSE`, `internal/third_party/topf/VENDORED.md`, `internal/third_party/imagefactory/schematic/`, `internal/third_party/imagefactory/LICENSE`, `internal/third_party/imagefactory/VENDORED.md`, `internal/third_party/topf/render/render_test.go`
- Modify: `go.mod`
- Modify: `go.sum`
- Modify: `.taskfiles/go.yml`

**Interfaces:**
- Produces (package `render` at `internal/third_party/topf/render`, the one entry point hephaestus calls; marked `// hephaestus: changed — new entry point`):
  ```go
  type Input struct {
      FS     fs.FS              // the cluster subpath; nothing outside it is read
      Values map[string]string  // whole reference string ("ref+op://…") → resolved value
      Funcs  template.FuncMap   // the template functions; Topf's sprig.TxtFuncMap() is not used
  }
  type Node struct {
      Host, Role, IP    string
      Config            []byte // the full per-node machine config, byte-identical to `topf render`
      SchematicID       string
      SchematicBody     []byte // canonical body for an @file schematic; nil for a literal id
      TalosVersion      string // with a leading "v"
      KubernetesVersion string
  }
  func Render(ctx context.Context, in Input) ([]Node, error)
  ```
- Produces (package `schematic` at `internal/third_party/imagefactory/schematic`): `func ID(body []byte) (string, error)` — the image-factory content-hash computation (canonical marshal, sha256 hex).
- Changes: `.taskfiles/go.yml` `go:ci` gains `machinery` (after `tidy:check`) and `noexec`:
  - `go:machinery`: `test "$(go list -m -f '{{.Version}}' github.com/siderolabs/talos/pkg/machinery)" = v1.14.4` and `! go list -m all | grep -E '^github.com/siderolabs/talos '`.
  - `go:noexec`: `! go list -deps ./internal/third_party/topf/... | grep -x os/exec`.

- [ ] **Step 1: Check out Topf at the full SHA** (never through the module proxy; its v0.6.2 is a different commit).

Run: `rtk proxy sh -c "mkdir -p build && rm -rf build/topf-src && git clone -q https://github.com/postfinance/topf build/topf-src && git -C build/topf-src checkout -q 70cdbc83cfbadc023ce4c7c0e3180eedfa577f16 && grep -E 'machinery|sprig|yaml/v4' build/topf-src/go.mod"`
Expected: `github.com/siderolabs/talos/pkg/machinery v1.14.4`, `github.com/Masterminds/sprig/v3 v3.3.0` and a `go.yaml.in/yaml/v4` line (its version is the one this plan pins).

- [ ] **Step 2: Write the failing test** in `internal/third_party/topf/render/render_test.go` over a minimal tree built in the test (`fstest.MapFS`): `topf.yaml` (`clusterName: lab`, `clusterEndpoint: https://10.10.20.11:6443`, `talosVersion: v1.12.11`, `kubernetesVersion: v1.35.10`, nodes `cp-01` controlplane `10.10.20.11` and `wrk-01` worker `10.10.20.21`), a `secrets.yaml` whose every value is a `ref+op://vault/item/<field>` reference, and `Values` mapping each reference to a fixed fixture value from a committed bundle (Step 3).
```go
func TestRender_TwoNodes(t *testing.T)            // len(nodes)==2; hosts cp-01, wrk-01; roles; Config non-empty and parses as YAML; TalosVersion "v1.12.11"
func TestRender_NeverReadsOutsideFS(t *testing.T)   // a topf.yaml naming "@../x.yaml" schematic → error; no os.Open outside FS (FS is a MapFS)
func TestRender_LiteralSchematicHasNoBody(t *testing.T) // schematicId "376567988ad3…b4ba" → SchematicBody nil, SchematicID equal
func TestRender_FileSchematicBodyAndID(t *testing.T)    // "@schematic.yaml" → SchematicID == schematic.ID(SchematicBody)
```
- [ ] **Step 3: Run to verify failure**

Run: `rtk proxy go test ./internal/third_party/topf/render/ -run TestRender -v`
Expected: FAIL, compile error (package or `Render` undefined).

- [ ] **Step 4: Vendor.** Copy from `build/topf-src` only the files the per-node render needs (`pkg/config`, the node-config generation in `internal/topf`, `internal/schematic`), flattened into `internal/third_party/topf/{config,render,schematic}` with import paths rewritten. In the copy, and recorded one per line in `VENDORED.md` (source URL, full SHA, licence MIT, each change):
  - remove every `os/exec` use: `vals`, `sops`, the nodes provider and the secrets provider; references are looked up in `Input.Values` (a missing reference is an error naming it);
  - remove the secrets-bundle prompt, `generateAndStoreSecrets` and `Put`: a missing or unparsable bundle is an error;
  - templates get `Input.Funcs`; the template data carries neither `DecryptCache` nor `PatchesDir`;
  - the image-factory client imports are replaced by `internal/third_party/imagefactory/schematic.ID`; Topf's import of the Talos main module's `pkg/images` is replaced by a local constant.
  Copy image-factory's schematic-ID computation (MPL-2.0; file headers kept; changes marked) into `internal/third_party/imagefactory/schematic` with its own `LICENSE` and `VENDORED.md`. Write the `testdata` bundle once with a throwaway `go run` program kept at `internal/third_party/topf/render/testdata/genbundle/main.go` (build tag `ignore`) using machinery's `secrets.NewBundle`; commit its output and the reference→value map. Add the dependencies of the table above. Add the two `go.yml` tasks.

- [ ] **Step 5: Run to verify pass**

Run: `rtk proxy go test ./internal/third_party/topf/... ./internal/third_party/imagefactory/... -race -v`
Expected: PASS.
Run: `rtk proxy task go:machinery go:noexec`
Expected: exit 0.

- [ ] **Step 6: Gate and commit**

Run: `DOCKER_HOST=unix://$HOME/.colima/default/docker.sock rtk proxy task ci`
Expected: exit 0; `0 issues.`; `No vulnerabilities found.`
Run: `rtk proxy git add internal/third_party/topf internal/third_party/imagefactory go.mod go.sum .taskfiles/go.yml` then `rtk proxy git -c commit.gpgsign=false commit -m "feat: vendor the Topf render path and the image-factory schematic id" -m "Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>"`
Expected: one new commit.

---

### Task 2: Migration 00003 and the fleet model columns
Kind: rewrite

**Files:**
- Create: `internal/db/migrations/sqlite/00003_clusters.sql`, `internal/db/migrations/postgres/00003_clusters.sql`
- Modify: `internal/db/migrations_test.go`
- Modify: `internal/fleet/machine.go`

**Interfaces:**
- Produces (schema; given in full because schema is expensive to retrofit). SQLite (`STRICT`, `TEXT` times, `INTEGER` booleans with `CHECK (x IN (0, 1))`); Postgres the same with `TIMESTAMPTZ`, `BOOLEAN`, `BYTEA` for `BLOB`, `BIGINT` for counters, partial indexes `WHERE … IS NOT NULL`, and one `ALTER TABLE machines` statement with named constraints. Down drops in reverse, indexes before the columns they cover.
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
  `talos_cache.assets` is JSON `{"kernel":{"size":N,"sha256":"…"},"initramfs":{…},"cmdline":{…}}`, `{}` before the first complete fetch. `last_error` holds factory/HTTP text made printable (it never holds a secret).
- Produces (package `fleet`, `machine.go`): `Machine` gains `IdentityObserved bool \`bun:"identity_observed"\``, `TalosSchematic *string \`bun:"talos_schematic"\``, `TalosVersion *string \`bun:"talos_version"\``.

- [ ] **Step 1: Write the failing tests** in `internal/db/migrations_test.go` (`runMigrateSuite`, both databases):
```go
// Tables subtest: expects clusters, secret_versions, cluster_pairs, host_renders, tickets, talos_cache
// new subtest "BindingUnique": two machines with the same (cluster_id, host_name) → second UPDATE fails; NULL pairs never conflict
// new subtest "OneLiveTicket": two tickets rows for one machine with consumed_at and revoked_at NULL → second INSERT fails; a revoked one plus a live one → ok
// new subtest "DriftPair": host_renders with drift=1 and drift_config_hash NULL → CHECK fails
// new subtest "TupleBothOrNeither": machines talos_schematic set, talos_version NULL → CHECK fails
// new subtest "Down": goose down to 00002 leaves no new table and no new machines column
```
- [ ] **Step 2: Run to verify failure**

Run: `rtk proxy go test ./internal/db/ -run TestMigrate -v`
Expected: FAIL (missing tables).

- [ ] **Step 3: Implement** both migration files and the three `Machine` fields.

- [ ] **Step 4: Run to verify pass**

Run: `rtk proxy go test ./internal/db/ ./internal/fleet/ -race -v`
Expected: PASS.
Run: `DOCKER_HOST=unix://$HOME/.colima/default/docker.sock rtk proxy go test -tags integration ./internal/db/ ./internal/fleet/ -race -run Postgres -v`
Expected: PASS, including `TestMigrate_Postgres/BindingUnique` and `/Down`.

- [ ] **Step 5: Gate and commit**

Run: `DOCKER_HOST=unix://$HOME/.colima/default/docker.sock rtk proxy task ci`
Expected: exit 0.
Run: `rtk proxy git add internal/db internal/fleet/machine.go` then `rtk proxy git -c commit.gpgsign=false commit -m "feat: schema for clusters, pairs, renders, tickets and the Talos cache" -m "Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>"`
Expected: one new commit.

---

### Task 3: Data key and 1Password token configuration, sealed fields and the canary
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
- Produces (package `main`, `config.go`): `rawConfig` gains `DataKey secret.Secret \`mapstructure:"data_key"\``, `DataKeyFile string \`mapstructure:"data_key_file"\``, `OP struct { Token secret.Secret \`mapstructure:"token"\`; TokenFile string \`mapstructure:"token_file"\` } \`mapstructure:"op"\``; `serveConfig` gains `DataKey *datakey.Key` and `OPToken secret.Secret` (empty when unset). Validation: exactly one of `HEPHAESTUS_DATA_KEY` / `HEPHAESTUS_DATA_KEY_FILE` ("set exactly one of HEPHAESTUS_DATA_KEY or HEPHAESTUS_DATA_KEY_FILE"); at most one of `HEPHAESTUS_OP_TOKEN` / `HEPHAESTUS_OP_TOKEN_FILE`; a `_FILE` is read once (trailing newline trimmed), and a read error names the variable, never the content; a bad key reports `datakey.Parse`'s error under the variable's name.

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

- [ ] **Step 3: Implement** the signatures above. Existing config tests and fixtures gain `HEPHAESTUS_DATA_KEY` set to the fixture key so every other config test still passes.

- [ ] **Step 4: Run to verify pass**

Run: `rtk proxy go test ./internal/datakey/ ./cmd/hephaestus/ -race -v`
Expected: PASS.

- [ ] **Step 5: Gate and commit**

Run: `DOCKER_HOST=unix://$HOME/.colima/default/docker.sock rtk proxy task ci`
Expected: exit 0.
Run: `rtk proxy git add internal/datakey cmd/hephaestus/config.go cmd/hephaestus/config_test.go` then `rtk proxy git -c commit.gpgsign=false commit -m "feat: data key and 1Password token from env or file, sealed fields and a key canary" -m "Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>"`
Expected: one new commit.

---

### Task 4: The render package: load rules, template allow-list, redaction and hashes

**Files:**
- Create: `internal/render/doc.go`, `internal/render/load.go`, `internal/render/load_test.go`, `internal/render/funcs.go`, `internal/render/funcs_test.go`, `internal/render/redact.go`, `internal/render/redact_test.go`, `internal/render/render.go`, `internal/render/render_test.go`, `internal/render/refs.go`, `internal/render/refs_test.go`, `internal/render/testdata/` (fixture trees)

**Interfaces:**
- Consumes: Task 1 `topfrender.Render`, `Input`, `Node`; `imagefactory/schematic.ID`.
- Produces (package `render`):
  ```go
  type RefusalError struct{ File, Rule string }        // Error(): "render: <file>: <rule>"; never a value
  type Cluster struct {
      FS                fs.FS
      Name              string
      TalosVersion      string   // "v1.12.11"
      KubernetesVersion string
      Hosts             []Host
      Refs              []string // every whole-scalar reference, sorted, deduplicated
  }
  type Host struct{ Name, Role, IP string }
  func Load(fsys fs.FS) (*Cluster, error)              // every load rule of spec §5.5 and this plan's §11.8 ruling; refusals are *RefusalError
  func FuncMap() template.FuncMap                      // the allow-list
  func AllowedFuncs() []string; func DeniedFuncs() []string
  type HostRender struct {
      Host, Role, IP           string
      Config                   []byte
      Redacted                 []byte
      ConfigHash, DisplayHash  string   // sha256 hex
      SchematicID              string
      SchematicBody            []byte
      TalosVersion             string
  }
  func (c *Cluster) Render(ctx context.Context, values map[string]string) ([]HostRender, error)
  func Redact(render []byte, values []string) ([]byte, error)   // fail-closed; ErrRedaction on survival or unparsable output
  var ErrRedaction = errors.New("render: a secret value survived redaction")
  func IsReference(s string) bool                              // whole-scalar "ref+op://VAULT/ITEM/[SECTION/]FIELD", no "?", "#", space or "+" after "ref+op://"
  func HasEmbeddedReference(s string) bool                     // unanchored `ref\+[A-Za-z0-9]+://`
  const Marker = "REDACTED"
  ```
- Load rules (each a `*RefusalError` with the rule text quoted here): `nodesProvider set`, `secretsProvider set`, `configDir set`, `secrets bundle missing`, `secrets bundle unparsable`, `duplicate host` (case-insensitive), `host not a DNS name` (labels of `[a-z0-9-]`, 1–63, joined by dots, ≤253, no trailing dot, lower case only), `non-plain data value` (any decoded value in `data` or `nodes[].data` that is not string, number, bool, null, list or map), `kubernetesVersion missing`, `talosVersion missing`, `talosVersion out of range` (release `v1.12.11` ≤ v ≤ `v1.14.x`, any pre-release suffix refused), `node talosVersion differs`, `factory not default`, `platform not metal`, `secureboot enabled`, `reference in host, ip, schematicId or talosVersion`, `reference not op` (any `ref+<scheme>://` other than `op`), `embedded reference`, `path escapes tree` (absolute, `..`, a symlink leaving the tree, a symlinked directory cycle, an `@` path), `sops metadata` (any file with a top-level `sops:` map), `template function not allowed: <name>`, `template reads PatchesDir`.
- Allow-list: built from `sprig.HermeticTxtFuncMap()` at v3.3.0 minus the denied set; `DeniedFuncs()` includes at least `ago`, `shuffle`, `randInt`, `durationRound`, `toDate`, `mustToDate`, `keys`, `values`, `mustDateModify`, `must_date_modify`, `unixEpoch`, `genPrivateKey`, `genCA`, `genCAWithKey`, `genSelfSignedCert`, `genSelfSignedCertWithKey`, `genSignedCert`, `genSignedCertWithKey`, `bcrypt`, `htpasswd`, `encryptAES`; every other name in the hermetic map is in exactly one of the two lists.
- Redaction (spec §5.5 and §11.5 ruling): replace longest pattern first, for each non-empty value: the value, its std and URL-safe base64, its lowercase hex, and each line of a multi-line value except lines shorter than 8 characters after trimming and PEM armour lines; then decode the output as YAML (multi-document) and fail closed if any decoded scalar still contains any pattern; an output that does not parse is `ErrRedaction`. The error names the reference, never the value.
- Schematic body check: `Render` renders a schematic `.tpl` a second time with every value replaced by the same-length dummy (`x`…, or `y`… when the value is all `x`); differing bodies, or a dummy render error, refuse with rule `secret reaches schematic`.

- [ ] **Step 1: Write the failing tests** (fixture trees under `internal/render/testdata/<case>/`, one directory per refusal):
```go
func TestLoad_Refusals(t *testing.T)            // one subtest per rule above: errors.AsType[*RefusalError](err) ok, Rule equal, File equal
func TestLoad_Valid(t *testing.T)               // testdata/valid: Name "lab", TalosVersion "v1.12.11", 2 hosts, Refs sorted
func TestLoad_PreRelease(t *testing.T)          // "v1.15.0-beta.1" and "v1.14.7-rc.0" refused with "talosVersion out of range"; "v1.14.6" ok; "1.13.12" ok (normalised "v1.13.12")
func TestFuncMap_PartitionsHermeticMap(t *testing.T) // every key of sprig.HermeticTxtFuncMap() is in exactly one of AllowedFuncs/DeniedFuncs; FuncMap() keys == AllowedFuncs()
func TestFuncMap_DeniedAbsent(t *testing.T)     // "keys", "values", "ago", "randInt", "toDate" not in FuncMap()
func TestFuncMap_RepeatableOutput(t *testing.T) // every allowed function called in a fixture template renders identically twice
func TestTemplateData_PlainOnly(t *testing.T)   // reflection over the template data type: every reachable method is in a reviewed allow-list (empty unless justified)
func TestRedact_MultiLineValue(t *testing.T)    // a PEM key value: no line of it remains; the armour lines remain
func TestRedact_Base64AndHex(t *testing.T)      // base64(value) in output → Marker
func TestRedact_LongestFirst(t *testing.T)      // values "abcdefgh" and "abcdefghijkl" → no "ijkl" tail remains
func TestRedact_YAMLQuoted(t *testing.T)        // value `@dm1n'sPass` emitted as 'p@dm1n''sPass' → caught (ErrRedaction or replaced)
func TestRedact_ShortValueRedacted(t *testing.T) // value "abc" → Marker in place
func TestRedact_EmptyValueIgnored(t *testing.T) // "" in values → output unchanged, no error
func TestRedact_UnparsableOutput(t *testing.T)  // → errors.Is(err, ErrRedaction)
func TestRender_Hashes(t *testing.T)            // ConfigHash == sha256(Config); DisplayHash == sha256(Redacted); same values → same hashes twice
func TestSchematicBody_TransformedSecretRefused(t *testing.T) // .tpl with {{ .Data.x | sha256sum }} where x is resolved → rule "secret reaches schematic"
func TestIsReference(t *testing.T)              // "ref+op://v/i/f" true; "ref+op://v/i/s/f" true; with "?attr=otp", "#x", " ", "+" false; "ref+vault://a" false
func FuzzIsReference(f *testing.F)              // never panics
func FuzzRedact(f *testing.F)                   // never panics; when err == nil no value of len ≥ 1 appears in a decoded scalar
```
- [ ] **Step 2: Run to verify failure**

Run: `rtk proxy go test ./internal/render/ -v`
Expected: FAIL, compile errors (`Load`, `Redact` undefined).

- [ ] **Step 3: Implement** the signatures above.

- [ ] **Step 4: Run to verify pass, then fuzz**

Run: `rtk proxy go test ./internal/render/ -race -v`
Expected: PASS.
Run: `rtk proxy go test ./internal/render/ -run '^$' -fuzz=FuzzRedact -fuzztime=30s`
Expected: PASS.
Run: `rtk proxy go test ./internal/render/ -run '^$' -fuzz=FuzzIsReference -fuzztime=30s`
Expected: PASS.

- [ ] **Step 5: Gate and commit**

Run: `DOCKER_HOST=unix://$HOME/.colima/default/docker.sock rtk proxy task ci`
Expected: exit 0.
Run: `rtk proxy git add internal/render` then `rtk proxy git -c commit.gpgsign=false commit -m "feat: render with load rules, a template allow-list and fail-closed redaction" -m "Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>"`
Expected: one new commit.

---

### Task 5: Golden Topf parity test
Kind: rewrite

**Files:**
- Create: `build/parity/Dockerfile`, `build/parity/valsstub/main.go`, `internal/render/parity_test.go`, `internal/render/testdata/parity/` (hermetic fixtures: no `env`, time or random functions, no `.PatchesDir`)
- Modify: `.taskfiles/go.yml`
- Modify: `taskfile.yml`

**Interfaces:**
- Produces: `task render:parity` (root `taskfile.yml`, needs `DOCKER_HOST`): builds `build/parity/Dockerfile` (base `golang:1.27.4-trixie@sha256:<digest>`, pinned by digest; `git clone https://github.com/postfinance/topf` and `git checkout 70cdbc83cfbadc023ce4c7c0e3180eedfa577f16`; `go build -o /usr/local/bin/topf ./cmd/topf`; asserts `go version -m /usr/local/bin/topf | grep 'github.com/siderolabs/talos/pkg/machinery.*v1.14.4'`; builds `valsstub` as `/usr/local/bin/vals`), then runs `topf render` over each fixture into `build/parity/out/<case>/` with the stub resolving `ref+op://` references from `internal/render/testdata/parity/<case>/values.json`. `go:ci` gains `render:parity` before `test`.
- Produces: `TestParity_Golden` (build tag `parity`, run by `render:parity` after the container step) compares each `build/parity/out/<case>/<host>` byte-for-byte with `render.Cluster.Render` of the same fixture and values.
- The stub `vals` reads `vals eval` input on stdin, replaces every whole-scalar `ref+op://…` with its fixture value (as Topf's `vals eval` would), writes the result, and fails on any unknown reference.

- [ ] **Step 1: Spike the reference build** (ruling §11.7).

Run: `DOCKER_HOST=unix://$HOME/.colima/default/docker.sock rtk proxy sh -c "docker build -f build/parity/Dockerfile -t hephaestus-parity:local . > build/parity-build.log 2>&1; echo exit=\$?; tail -3 build/parity-build.log"`
Expected: `exit=0`, and the log shows the `go version -m` assertion line matching `v1.14.4`. A failure that is not a network error is `PLAN: BLOCKED — every path is a guess: parity reference build`.

- [ ] **Step 2: Write the failing test** `TestParity_Golden` with two fixtures: `lab` (two hosts, patches in `all/`, `control-plane/`, `worker/`, a `node/cp-01/` patch, a `.yaml.tpl` using `.Data`) and `literal-schematic`.

- [ ] **Step 3: Run to verify failure**

Run: `DOCKER_HOST=unix://$HOME/.colima/default/docker.sock rtk proxy task render:parity`
Expected: FAIL (fixture outputs missing or `TestParity_Golden` undefined until written; after writing, FAIL until the task wiring exists).

- [ ] **Step 4: Implement** the task, the stub, and whatever `render` change parity reveals (record each such change in the report; a change in the vendored copy is recorded in `VENDORED.md`).

- [ ] **Step 5: Run to verify pass**

Run: `DOCKER_HOST=unix://$HOME/.colima/default/docker.sock rtk proxy task render:parity`
Expected: PASS, both fixtures byte-identical.

- [ ] **Step 6: Gate and commit**

Run: `DOCKER_HOST=unix://$HOME/.colima/default/docker.sock rtk proxy task ci`
Expected: exit 0.
Run: `rtk proxy git add build/parity internal/render .taskfiles/go.yml taskfile.yml` then `rtk proxy git -c commit.gpgsign=false commit -m "test: golden parity with topf render at the pinned commit" -m "Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>"`
Expected: one new commit.

---

### Task 6: The secrets package: 1Password resolver, error classes and the request counter
Kind: rewrite

**Files:**
- Create: `internal/secrets/doc.go`, `internal/secrets/onepassword.go`, `internal/secrets/onepassword_test.go`, `internal/secrets/counter.go`, `internal/secrets/counter_test.go`, `internal/secrets/smoke_test.go` (build tag `sdksmoke`)
- Modify: `go.mod`
- Modify: `go.sum`
- Modify: `.taskfiles/go.yml`

**Interfaces:**
- Produces (package `secrets`):
  ```go
  type Result struct { Value secret.Secret; Err error }   // per reference
  var ErrRateLimited = errors.New("secrets: 1Password rate limit reached")
  type WholeCallError struct{ Err error; Panicked bool }   // transport, token, client creation; Panicked when the text carries "recovered by wazero"
  type OnePassword struct{ /* token, integration name/version, lazily created *onepassword.Client, mutex */ }
  func NewOnePassword(token secret.Secret, version string) *OnePassword
  func (o *OnePassword) ResolveAll(ctx context.Context, refs []string) (map[string]Result, error)
  // nil error: per-reference Results (SDK v0.4.1 types.go:815-834); *WholeCallError for whole-call failures; errors.Is(err, ErrRateLimited) for RateLimitExceededError
  type Counter struct{ /* hour buckets over a rolling 24 h, mutex */ }
  func NewCounter(next http.RoundTripper, now func() time.Time) *Counter
  func (c *Counter) RoundTrip(r *http.Request) (*http.Response, error)   // counts a request whose host ends in .1password.com, .1password.ca or .1password.eu, then delegates
  func (c *Counter) Last24h() int
  ```
  The client is created on the first `ResolveAll`, under the mutex, and reused; a creation failure is a `*WholeCallError` and the next call tries again. A credential is resolved as `ResolveAll(ctx, []string{ref})`.
- Changes: `go:ci` gains `sdk-smoke`: `CGO_ENABLED=0 go test -tags sdksmoke ./internal/secrets/ -run TestOnePassword_SmokeDummyToken -v`.

- [ ] **Step 1: Write the failing tests** (the SDK is exercised only in the smoke test; unit tests use an injected resolve function via an unexported field set in the test file):
```go
func TestOnePassword_PerReferenceResults(t *testing.T) // fake returns {"ref+op://v/i/a": value "1", "ref+op://v/i/b": error} → map has both; err nil
func TestOnePassword_WholeCallError(t *testing.T)      // fake returns a transport error → errors.AsType[*WholeCallError] ok, Panicked false
func TestOnePassword_PanicText(t *testing.T)           // error text "… (recovered by wazero) …" → Panicked true
func TestOnePassword_RateLimited(t *testing.T)         // fake returns onepassword.RateLimitExceededError → errors.Is(err, ErrRateLimited)
func TestOnePassword_LazyClientRetried(t *testing.T)   // first creation fails → WholeCallError; second call creates and succeeds
func TestCounter_CountsOnlyOnePasswordHosts(t *testing.T) // my.1password.com, my.1password.ca counted; factory.talos.dev not
func TestCounter_RollingWindow(t *testing.T)           // 3 at t0, 2 at t0+23h → 5; at t0+24h+1s → 2
func TestOnePassword_SmokeDummyToken(t *testing.T)     // sdksmoke tag: token "ops_hephaestus-smoke-test"; ResolveAll returns a non-nil error within 60 s; no panic
```
- [ ] **Step 2: Run to verify failure**

Run: `rtk proxy go test ./internal/secrets/ -v`
Expected: FAIL, compile errors.

- [ ] **Step 3: Implement**; add `github.com/1password/onepassword-sdk-go@v0.4.1`.

- [ ] **Step 4: Run to verify pass**

Run: `rtk proxy go test ./internal/secrets/ -race -v`
Expected: PASS.
Run: `rtk proxy task go:sdk-smoke`
Expected: PASS within 60 s. (A failure because the network is unreachable is reported as such and retried once; a panic or a build failure under `CGO_ENABLED=0` is a real failure.)

- [ ] **Step 5: Gate and commit**

Run: `DOCKER_HOST=unix://$HOME/.colima/default/docker.sock rtk proxy task ci`
Expected: exit 0.
Run: `rtk proxy git add internal/secrets go.mod go.sum .taskfiles/go.yml` then `rtk proxy git -c commit.gpgsign=false commit -m "feat: 1Password resolver with error classes and a request counter" -m "Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>"`
Expected: one new commit.

---

### Task 7: Fleet bindings, transaction-scoped exports and identity observation
Kind: rewrite

**Files:**
- Modify: `internal/fleet/repository.go`
- Modify: `internal/fleet/transition.go`
- Modify: `internal/fleet/repository_test.go`
- Modify: `internal/fleet/transition_test.go`
- Create: `internal/fleet/binding.go`, `internal/fleet/binding_test.go`, `internal/fleet/binding_integration_test.go`

**Interfaces:**
- Produces (package `fleet`):
  ```go
  const ActorCLI = "cli"; const ActorSystem = "system"      // the unexported actorCLI/actorSystem become these
  type AuditEvent struct { At time.Time; Actor, Action, MachineID string; Detail any }
  func InsertAudit(ctx context.Context, q bun.IDB, ev AuditEvent) error
  type Binding struct { ClusterID, Host, TalosSchematic, TalosVersion string }
  // TransitionRequest gains: Bind *Binding   (required for approve, adopt, adopt_by_uuid, rebind; ignored otherwise)
  func TransitionTx(ctx context.Context, q bun.IDB, req TransitionRequest) (Machine, error)   // Transition runs it in RunInTx
  type AdoptByUUIDRequest struct { UUID string; Arch Arch; Bind Binding; Actor string; At time.Time }
  var ErrUUIDExists = errors.New("fleet: a machine with this UUID already exists")
  func AdoptByUUIDTx(ctx context.Context, q bun.IDB, req AdoptByUUIDRequest) (Machine, error)
  func HostBoundTx(ctx context.Context, q bun.IDB, clusterID, host string) (bool, error)
  func MachinesInClusterTx(ctx context.Context, q bun.IDB, clusterID string) ([]Machine, error)
  func SetTalosTupleTx(ctx context.Context, q bun.IDB, machineID, schematic, version string) error
  func (r *Repository) ListMachines(ctx context.Context) ([]Machine, error)   // ordered by created_at, id; MACs hydrated
  ```
- Binding columns per trigger (spec §5.2 table): `approve`, `adopt`, `adopt_by_uuid`, `rebind` set `cluster_id`, `host_name`, `talos_schematic`, `talos_version` from `Bind`; `unassign` and `unbind` set all four NULL; `host_left` keeps them; `forget` deletes the row; every other trigger leaves them. `rebind` with `Junk` true is refused with reason `junk_uuid` (a new guard in `Next`). `HostUnbound` for approve/adopt/rebind is computed by the caller with `HostBoundTx`; the unique index is the final word (a violation returns `*TransitionError` with reason `host_bound`).
- Identity observation: `AdoptByUUIDTx` stores `serial = ''`, `product = ''`, `identity_observed = false`, no MACs. In `observe` and `recordKnownBoot`, a machine with `IdentityObserved == false` is never marked junk or conflicted; its serial and product are written with `UPDATE … WHERE id = ? AND identity_observed = false`, setting it true, and audited `identity.observed`; a 0-row result reloads the machine and applies the normal comparison.

- [ ] **Step 1: Write the failing tests** (suite `runBindingSuite(t, open)`; `TestBinding_SQLite`, `TestBinding_Postgres`):
```go
// ApproveSetsBinding: approve with Bind{lab, cp-01, plain id, v1.12.11} → all four columns set; audit "transition.approve"
// UnassignClearsBinding, UnbindClearsBinding: all four NULL
// HostLeftKeepsBinding: columns unchanged, state orphaned
// CordonKeepsBinding
// ConcurrentApproveOneHost: dbtest.SQLitePair / PostgresPair: two approves into lab/cp-01 at once → exactly one succeeds; the other errors.AsType[*TransitionError] Reason "host_bound"
// RebindRefusesJunk: orphaned junk machine → reason "junk_uuid"
// RebindOwnFormerHost: orphaned from lab/cp-01, host back → rebind to lab/cp-01 ok
// AdoptByUUIDCreates: state installed, IdentityObserved false, Serial "", MACs empty, binding set
// AdoptByUUIDJunkRefused: "00000000-0000-0000-0000-000000000000" → error
// AdoptByUUIDExisting: existing UUID → errors.Is(err, ErrUUIDExists)
// FirstReportFillsIdentity: adopted-by-UUID machine, a boot with serial "S1" product "P1" → stored, IdentityObserved true, Junk false, audit "identity.observed"
// SecondReportAfterObservationConflicts: then a boot with serial "S2" → junk marked as today
// ConcurrentFirstReports: two first reports at once → one fills, the other compares and marks junk only if different
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
Expected: PASS, including `TestBinding_Postgres/ConcurrentApproveOneHost`.

- [ ] **Step 5: Gate and commit**

Run: `DOCKER_HOST=unix://$HOME/.colima/default/docker.sock rtk proxy task ci`
Expected: exit 0; no `not executed:` line.
Run: `rtk proxy git add internal/fleet` then `rtk proxy git -c commit.gpgsign=false commit -m "feat: host bindings, create-on-adopt and first-report identity" -m "Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>"`
Expected: one new commit.

---

### Task 8: Tickets
Kind: rewrite

**Files:**
- Create: `internal/fleet/ticket_repository.go`, `internal/fleet/ticket_repository_test.go`, `internal/fleet/ticket_repository_integration_test.go`
- Modify: `internal/fleet/repository.go`

**Interfaces:**
- Produces (package `fleet`):
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
  func (r *Repository) MintTicket(ctx context.Context, req MintRequest) (token string, t Ticket, err error)
  func RevokeMachineTicketsTx(ctx context.Context, q bun.IDB, machineID string, at time.Time) (int, error)
  func RevokeClusterTicketsTx(ctx context.Context, q bun.IDB, clusterID string, at time.Time) (int, error)
  func (r *Repository) TicketByToken(ctx context.Context, token string) (Ticket, bool, error)
  func (r *Repository) MarkTicketRequested(ctx context.Context, id string, at time.Time) error
  type RecordConfigRequest struct { TicketID string; At time.Time }
  type RecordConfigResult struct { Recorded bool; AlreadyConsumed bool; Machine Machine }
  func (r *Repository) RecordConfigServed(ctx context.Context, req RecordConfigRequest) (RecordConfigResult, error)
  func (r *Repository) PruneTickets(ctx context.Context, before time.Time) (int, error)
  func (r *Repository) PendingTickets(ctx context.Context, mintedBefore time.Time) ([]Ticket, error)   // live and unexpired, minted before the given time
  ```
- `MintTicket`: one transaction: revoke the machine's live tickets (any expiry), insert the new one; token = `base64.RawURLEncoding` of 32 bytes from `crypto/rand` (43 characters); the row stores `hex(sha256(token))`; retried once on a unique-index violation.
- Revocation hooks: `TransitionTx` revokes the machine's live tickets on every successful transition except `config_served`; `markUUIDJunk` revokes the live tickets of every machine with that UUID.
- `RecordConfigServed`: one transaction: load the ticket; already consumed → `AlreadyConsumed`; the machine is `assigned` and bound to the ticket's cluster and host (a revocation after the request was checked does not block it, spec §5.8 option A) → consume, `config_served` transition, revoke every other live ticket of the machine, audit `config.served` with detail `{ticket_id, pair_id}`; otherwise `Recorded` false.

- [ ] **Step 1: Write the failing tests** (suite `runTicketSuite`; `TestTickets_SQLite`, `TestTickets_Postgres`):
```go
// MintStoresHashOnly: token length 43; row token_sha256 == hex(sha256(token)); no column equals the token
// MintRevokesPrevious: second mint → first RevokedAt set
// ExpiredLiveTicketRevokedByMint: a ticket past ExpiresAt, unconsumed → next mint succeeds and revokes it
// ConcurrentMint: two mints at once → one live ticket remains
// TransitionRevokes: cordon → live ticket revoked; config_served does not revoke
// ClusterRevoke: RevokeClusterTicketsTx revokes both machines' tickets of cluster lab only
// JunkMarkRevokes: a conflicting boot marks the UUID junk → its machine's ticket revoked
// RecordAfterRevocation: revoke, then RecordConfigServed → Recorded true, state installing (option A)
// RecordAlreadyConsumed: second call → AlreadyConsumed true
// RecordMachineUnassigned: unassigned meanwhile → Recorded false
// RecordRevokesOthers
// PruneAfterSevenDays, PendingTickets
```
- [ ] **Step 2: Run to verify failure**

Run: `rtk proxy go test ./internal/fleet/ -run TestTickets -v`
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

### Task 9: The Talos cache repository
Kind: rewrite

**Files:**
- Create: `internal/fleet/talos_repository.go`, `internal/fleet/talos_repository_test.go`, `internal/fleet/talos_repository_integration_test.go`
- Modify: `internal/fleet/repository.go`

**Interfaces:**
- Produces (package `fleet`):
  ```go
  type TalosKey struct { Schematic, Version string; Arch Arch }
  type TalosAsset struct { Size int64 `json:"size"`; SHA256 string `json:"sha256"` }
  type TalosAssets map[string]TalosAsset          // keys "kernel", "initramfs", "cmdline"
  type TalosRow struct { bun.BaseModel `bun:"table:talos_cache"`; Schematic, Version string; Arch Arch; State CacheState; Body []byte; Attempts int; NextAttemptAt time.Time; BytesDone, BytesTotal int64; LastError string; Assets TalosAssets; UpdatedAt time.Time }
  func (r TalosRow) Key() TalosKey
  type TalosWant struct { Key TalosKey; Body []byte; Rearm bool }
  func WantTalosTx(ctx context.Context, q bun.IDB, w TalosWant, at time.Time) (TalosRow, error)
  func (r *Repository) RearmTalos(ctx context.Context, key TalosKey, at time.Time) (bool, error)
  type TalosClass int; const ( TalosAssigned TalosClass = iota; TalosOther )
  func (r *Repository) ClaimTalos(ctx context.Context, class TalosClass, at time.Time) (TalosRow, bool, error)
  func (r *Repository) RecordTalosProgress(ctx context.Context, key TalosKey, done, total int64) error
  func (r *Repository) MarkTalosReady(ctx context.Context, key TalosKey, assets TalosAssets, at time.Time) (bool, error)
  type TalosFailure struct { Key TalosKey; Cause string; Integrity bool; At time.Time }
  func (r *Repository) RecordTalosFailure(ctx context.Context, f TalosFailure) (TalosRow, error)
  func (r *Repository) ResetTalosFetching(ctx context.Context, at time.Time) (int, error)
  func (r *Repository) ResetTalosFetchingRow(ctx context.Context, key TalosKey, at time.Time) (bool, error)
  func (r *Repository) RequeueTalos(ctx context.Context, key TalosKey, at time.Time) (bool, error)        // ready → wanted
  func (r *Repository) ResetTalosHash(ctx context.Context, key TalosKey, actor string, at time.Time) (bool, error)   // clears assets; any state → wanted; attempts 0; audit talos.hash_reset
  func (r *Repository) TalosRow(ctx context.Context, key TalosKey) (TalosRow, bool, error)
  func (r *Repository) TalosRows(ctx context.Context) ([]TalosRow, error)
  ```
- `WantTalosTx`: absent → insert `wanted` with `Body`; present without a body and `w.Body != nil` → set the body; `Rearm` and state `failed` or `broken` → `wanted`, attempts 0, `next_attempt_at` NULL. Row 8's use passes `Rearm: false`.
- `ClaimTalos`: `TalosAssigned` claims only rows whose key equals `(talos_schematic, talos_version, arch)` of some machine in `assigned`; `TalosOther` claims any other claimable row; claimable = `wanted`, or `failed` with `next_attempt_at <= at`; oldest `updated_at` first; compare-and-swap to `fetching`.
- Failures as `utility_cache`: attempts+1, `last_error` via the existing `storedCause`, integrity or the 10th failure → `broken` with audit `talos.broken` and alert kind `talos_broken`; otherwise `failed` with `Backoff(attempts)`.
- `TransitionTx` re-arm hook: a transition that lands in `assigned` (reinstall, abort, uncordon) calls `RearmTalos` for the machine's tuple and arch inside the same transaction when the tuple is set (it never creates a row; it cannot read the body).

- [ ] **Step 1: Write the failing tests** (suite `runTalosSuite`; `TestTalosCache_SQLite`, `TestTalosCache_Postgres`):
```go
// WantCreates, WantSetsMissingBody, WantKeepsExistingBody, WantRearmsBroken, WantWithoutRearmKeepsBroken
// ClaimAssignedOnly: two rows, one matching an assigned machine's tuple → TalosAssigned claims it; TalosOther claims the other
// ClaimRespectsBackoff
// TenthFailureBroken, IntegrityBroken (audit talos.broken)
// ReadyStoresAssets: MarkTalosReady with three assets → Assets round-trips
// ResetHashFromReady: ready row with assets → ResetTalosHash → wanted, Assets empty, audit talos.hash_reset
// AbortRearms: a machine aborts into assigned with a broken row for its tuple → row wanted
// ResetFetching, RequeueReady
```
- [ ] **Step 2: Run to verify failure**

Run: `rtk proxy go test ./internal/fleet/ -run TestTalosCache -v`
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

### Task 10: The clusters repository: definitions, pairs, versions, renders and the processing mark
Kind: rewrite

**Files:**
- Create: `internal/clusters/doc.go`, `internal/clusters/repository.go`, `internal/clusters/repository_test.go`, `internal/clusters/repository_integration_test.go`, `internal/clusters/main_integration_test.go`
- Modify: `.taskfiles/go.yml`

**Interfaces:**
- Consumes: Task 3 `datakey.Field`, `SealField`, `OpenField`; Task 4 `render.IsReference`, `render.HostRender`; Task 7 `fleet.InsertAudit`.
- Produces (package `clusters`):
  ```go
  var ( ErrNotFound = errors.New("clusters: not found"); ErrClusterInUse = errors.New("clusters: machines are bound to this cluster"); ErrStale = errors.New("clusters: the pair is no longer pending") )
  type Cluster struct { /* every clusters column except sealed ones, with bun tags; sealed columns as []byte fields */ }
  type Repository struct { /* *bun.DB, *datakey.Key */ }
  func NewRepository(d *db.DB, key *datakey.Key) *Repository
  type ImportRequest struct { Name, RepoURL, Ref, Subpath, Username, CredentialRef string; At time.Time }
  func (r *Repository) Import(ctx context.Context, req ImportRequest) (Cluster, error)   // name a DNS label; https URL without userinfo; credential empty or render.IsReference; audit cluster.imported
  type SetRequest struct { Name string; RepoURL, Ref, Subpath, Username, CredentialRef *string; At time.Time }
  func (r *Repository) Set(ctx context.Context, req SetRequest) (Cluster, error)          // revision+1; a changed repo/ref/path clears pending pair, failed tree, processing mark and last_head_tree; a changed credential clears credential_sealed
  func (r *Repository) Remove(ctx context.Context, name string, actor string, at time.Time) error   // ErrClusterInUse while a machine has cluster_id = id
  func (r *Repository) Get(ctx context.Context, name string) (Cluster, error)
  func (r *Repository) List(ctx context.Context) ([]Cluster, error)
  func (r *Repository) RequestRefresh(ctx context.Context, name string, at time.Time) error   // refresh_requested=1, attempts 0, next_attempt_at NULL, rate_limited_until NULL
  type Trigger string; const ( TriggerHead Trigger = "head"; TriggerRefresh Trigger = "refresh"; TriggerDaily Trigger = "daily"; TriggerRetryFailed Trigger = "retry_failed" )
  type MarkRequest struct { ClusterID, Tree string; Revision int64; Trigger Trigger; At time.Time }
  func (r *Repository) MarkProcessing(ctx context.Context, req MarkRequest) (bool, error)   // conditional on revision and no processing mark; consumes the trigger (refresh_requested=0, or last_daily_at=At)
  func (r *Repository) ClearProcessing(ctx context.Context, clusterID string) error          // graceful shutdown
  func (r *Repository) ConvertInterrupted(ctx context.Context, at time.Time) (int, error)    // processing → failed tree "interrupted", last_head_tree = tree, mark cleared
  type StorePendingRequest struct { ClusterID, Tree string; Revision int64; Values map[string]string; TalosVersion string; Renders []render.HostRender; At time.Time }
  func (r *Repository) StorePending(ctx context.Context, req StorePendingRequest) (pairID string, stored bool, err error)
  type StoreFailedRequest struct { ClusterID, Tree string; Revision int64; Error string; At time.Time }
  func (r *Repository) StoreFailed(ctx context.Context, req StoreFailedRequest) (bool, error)
  func (r *Repository) FailedError(ctx context.Context, clusterID string) (string, error)
  func (r *Repository) RecordFetch(ctx context.Context, clusterID string, fetchErr error, at time.Time) error   // error text sealed
  func (r *Repository) RecordWholeCallFailure(ctx context.Context, clusterID string, rateLimited bool, nextDaily time.Time, at time.Time) error   // attempts+1, next_attempt_at = at+fleet.Backoff(attempts); rateLimited → rate_limited_until = nextDaily
  func (r *Repository) ResetAttempts(ctx context.Context, clusterID string) error
  func (r *Repository) SetCredential(ctx context.Context, clusterID string, value secret.Secret, at time.Time) error
  func (r *Repository) Credential(ctx context.Context, clusterID string) (secret.Secret, bool, error)
  type Pair struct { ID, ClusterID, TreeHash, SecretVersionID, TalosVersion string; CreatedAt, ApprovedAt time.Time; ApprovedBy string }
  func (r *Repository) Pair(ctx context.Context, id string) (Pair, error)
  func (r *Repository) Values(ctx context.Context, versionID string) (map[string]string, error)   // opens the sealed version
  type HostRow struct { /* host_renders columns; Redacted and DriftRedacted opened on read */ }
  func (r *Repository) HostRows(ctx context.Context, pairID string) ([]HostRow, error)
  ```
- `StorePending` / `StoreFailed`: one transaction, locking the `clusters` row first, conditional on `revision = req.Revision AND processing_tree = req.Tree`; `StorePending` inserts the version (sealed JSON of `Values`, field `{secret_versions, <id>, values_sealed}`), then the pair, then the renders (`redacted_sealed` field `{host_renders, <pair id>/<host>, redacted_sealed}`, `schematic_body` plain: it passed the secret check), deletes the older pending pair (renders, pair, then its version), sets `pending_pair_id`, `last_head_tree = tree`, clears the processing mark and the failed tree; `StoreFailed` seals `Error`, sets `failed_tree`, `last_head_tree = tree`, clears the mark. A non-matching condition returns `false`, writes nothing.
- `.taskfiles/go.yml`: `DB_PKGS` becomes `./internal/db/... ./internal/fleet/... ./internal/clusters/...`.

- [ ] **Step 1: Write the failing tests** (suite `runRepositorySuite`; `TestRepository_SQLite`, `TestRepository_Postgres`):
```go
// ImportValid, ImportRefusesHTTP, ImportRefusesUserinfo ("https://u:p@host/r"), ImportRefusesBadName ("Lab", "a/b"), ImportRefusesBadCredential
// SetChangedSourceClears: pending pair and failed tree set; Set RepoURL → both cleared, processing and last_head_tree NULL, revision+1
// RemoveInUse → errors.Is(err, ErrClusterInUse); RemoveFree → gone (cascade)
// MarkConsumesRefresh: refresh_requested 1 → MarkProcessing(TriggerRefresh) → 0
// MarkRefusedOnStaleRevision
// StorePendingReplacesOlder: second StorePending deletes first pair, its renders and its version
// StorePendingStaleRevision: Set between Mark and Store → stored false, nothing written
// StoreFailedSealsError: failed_error_sealed opens to the text; no column contains the plaintext
// ConvertInterrupted: processing tree T → failed_tree T, FailedError "interrupted", last_head_tree T, mark cleared
// ClearProcessingKeepsLastHead
// WholeCallBackoff, RateLimitedUntilNextDaily, ResetAttempts
// CredentialRoundTrip
// ValuesRoundTrip
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
Run: `rtk proxy git add internal/clusters .taskfiles/go.yml` then `rtk proxy git -c commit.gpgsign=false commit -m "feat: cluster definitions, pairs, sealed versions and the processing mark" -m "Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>"`
Expected: one new commit.

---

### Task 11: The git store
Kind: rewrite

**Files:**
- Create: `internal/clusters/git.go`, `internal/clusters/git_test.go`, `internal/clusters/memfs.go`, `internal/clusters/memfs_test.go`
- Modify: `go.mod`
- Modify: `go.sum`

**Interfaces:**
- Produces (package `clusters`, unexported; Task 12 uses them):
  ```go
  type gitSource struct { URL, Ref, Subpath, Username string; Password secret.Secret }
  type gitStore struct { /* dir string; mu sync.Mutex; lastMovedAside time.Time; now func() time.Time; caBundle []byte (tests only) */ }
  func newGitStore(dir string, now func() time.Time) *gitStore
  func (g *gitStore) fetch(ctx context.Context, clusterID string, src gitSource) (tree string, err error)   // full fetch (no depth) into <dir>/<clusterID>.git (bare); ref = branch name; returns the subpath tree hash
  func (g *gitStore) readTree(ctx context.Context, clusterID, tree string) (fs.FS, error)                 // in-memory fs of every file under the tree, symlinks kept as symlinks (fs.ReadLinkFS)
  func (g *gitStore) moveAside(at time.Time) (moved string, ok bool, err error)                           // renames <dir> to <dir>.corrupt-<UTC 20060102T150405Z>, keeps only the newest; ok false within an hour of the last move
  func gitCorrupt(err error) bool
  type memFS struct { /* map[string]memFile */ }   // implements fs.FS, fs.ReadDirFS, fs.ReadLinkFS
  ```
  Every method holds `g.mu`. Credentials are `http.BasicAuth{Username, Password.Expose()}`. A missing subpath is an error naming it. `gitCorrupt` is true for `plumbing.ErrObjectNotFound` and for the packfile and index decode sentinels found in Step 1.

- [ ] **Step 1: List go-git's corruption sentinels.**

Run: `rtk proxy sh -c "go get github.com/go-git/go-git/v5@v5.19.3 && go doc github.com/go-git/go-git/v5/plumbing/format/packfile | grep -E '^\s*Err' ; go doc github.com/go-git/go-git/v5/plumbing/format/idxfile | grep -E '^\s*Err'"`
Expected: a list of `Err…` variables. Put every one whose doc says the data is malformed, invalid or unexpected in `gitCorrupt`, and name them in the report.

- [ ] **Step 2: Write the failing tests.** Repositories are made with the host `git` in `t.TempDir()` and served over TLS 1.3 by `httptest.NewUnstartedServer` running `git http-backend` through `net/http/cgi` (`GIT_PROJECT_ROOT`, `GIT_HTTP_EXPORT_ALL`); the store trusts the server through its test-only CA bundle (go-git `FetchOptions.CABundle`).
```go
func TestGit_FetchReturnsSubtreeHash(t *testing.T)       // commit A with lab/ → tree hash equals `git rev-parse A:lab`
func TestGit_CommitOutsideSubpathSameTree(t *testing.T)  // commit B touching other/ only → same tree hash
func TestGit_ReadTreeFiles(t *testing.T)                 // topf.yaml bytes equal; a symlink reads back with fs.ReadLink
func TestGit_BasicAuth(t *testing.T)                     // server requiring user "deploy" + password → fetch ok with them, error without
func TestGit_MissingSubpath(t *testing.T)
func TestGit_CorruptPackMovesAside(t *testing.T)         // truncate a pack file → readTree error; gitCorrupt(err) true; moveAside ok; second moveAside within the hour ok=false
func TestGit_FullHistoryKept(t *testing.T)               // after fetching A then C, readTree(A's tree) still works
func TestMemFS_FSTest(t *testing.T)                      // testing/fstest.TestFS passes
```
- [ ] **Step 3: Run to verify failure**

Run: `rtk proxy go test ./internal/clusters/ -run 'TestGit|TestMemFS' -v`
Expected: FAIL, compile errors.

- [ ] **Step 4: Implement.**

- [ ] **Step 5: Run to verify pass**

Run: `rtk proxy go test ./internal/clusters/ -race -v`
Expected: PASS.

- [ ] **Step 6: Gate and commit**

Run: `DOCKER_HOST=unix://$HOME/.colima/default/docker.sock rtk proxy task ci`
Expected: exit 0.
Run: `rtk proxy git add internal/clusters go.mod go.sum` then `rtk proxy git -c commit.gpgsign=false commit -m "feat: git object store with subtree revisions and a corrupt-store move-aside" -m "Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>"`
Expected: one new commit.

---

### Task 12: The clusters service: revisions, refresh, the tree cache and drift
Kind: rewrite

**Files:**
- Create: `internal/clusters/service.go`, `internal/clusters/service_test.go`, `internal/clusters/checks.go`, `internal/clusters/checks_test.go`
- Modify: `internal/clusters/repository.go`

**Interfaces:**
- Consumes: Tasks 4, 6 (`secrets.Result`, `secrets.WholeCallError`, `secrets.ErrRateLimited`, `secrets.Counter`), 10, 11.
- Produces (package `clusters`):
  ```go
  type Resolver interface { ResolveAll(ctx context.Context, refs []string) (map[string]secrets.Result, error) }   // consumer-side; *secrets.OnePassword and the test double implement it
  const ( PollInterval = 10 * time.Second; FetchInterval = 5 * time.Minute; FetchDeadline = 5 * time.Minute; ResolveDeadline = 2 * time.Minute; DailyInterval = 24 * time.Hour; ClientWarnFailures = 3 )
  var ( ErrNoRender = errors.New("clusters: no render for this pair"); ErrDrift = errors.New("clusters: render differs from the approved hash") )
  type Config struct {
      Repo      *Repository
      Fleet     *fleet.Repository
      Resolver  Resolver          // nil when HEPHAESTUS_OP_TOKEN is unset
      Counter   *secrets.Counter  // nil allowed
      GitDir    string
      Now       func() time.Time
      OnError   func(context.Context, error)
      OnAlert   func(ctx context.Context, kind string)
  }
  func NewService(cfg Config) (*Service, error)
  func (s *Service) Startup(ctx context.Context) error   // ConvertInterrupted; fill the tree cache for every approved and pending pair; render each approved pair and set drift where the hash differs
  func (s *Service) Run(ctx context.Context) error       // until ctx ends; on exit clears any processing mark it holds (ClearProcessing)
  func (s *Service) Render(ctx context.Context, t fleet.Ticket) ([]byte, error)   // the ticket's pair and version only; ErrNoRender on a cache miss or a missing pair; ErrDrift after setting drift
  func (s *Service) Checks() []admin.Check
  ```
- `Run` each `PollInterval`, per cluster: resolve a missing credential (a per-reference failure waits for a trigger; a whole-call failure backs off); fetch when new, changed, or `FetchInterval` elapsed (deadline `FetchDeadline`; corrupt → `moveAside`, alert `git_store_moved_aside`, refill the tree cache for approved and pending pairs after the re-clone); a head tree ≠ `last_head_tree` → process it (`MarkProcessing(TriggerHead)` → `readTree` into the cache → `render.Load` → `ResolveAll(Refs)` → `Render` → `StorePending`, or `StoreFailed` with the error text); then triggers in this order: `refresh_requested`, the daily timer (`last_daily_at` + `DailyInterval`), honouring `next_attempt_at` and `rate_limited_until`. A refresh is spec §5.4's two steps: retry the failed tree if any (if it now succeeds it becomes the pending pair); otherwise, or if that fails, resolve the newest good tree (pending's, else approved's) and replace the pending pair when any value differs. A per-reference failure marks `refresh_incomplete` and creates nothing. Every piece of work is wrapped by `MarkProcessing` and ends in `StorePending`/`StoreFailed`/`ClearProcessing`.
- Whole-call failures (`*secrets.WholeCallError`) call `RecordWholeCallFailure`; `errors.Is(err, secrets.ErrRateLimited)` passes `rateLimited` and the next daily time. A process-wide counter of consecutive whole-call failures feeds `secrets:client`.
- Checks: `secrets:cache` (warn: a cluster `refresh_incomplete`, or approved and no successful refresh for 2 days), `secrets:token` (warn: clusters exist and `Resolver` is nil), `secrets:budget` (warn: `Counter.Last24h() >= 500`), `secrets:client` (warn: consecutive whole-call failures ≥ `ClientWarnFailures`; output says a restart is the recovery when the last error had `Panicked`; clears on the next success), `git:fetch` (warn: last fetch failed, older than 30 min, or a failed tree; output names the cluster), `render:approved` (critical: the current approved pair of some cluster could not be rendered at its last attempt; keyed on that pair id).

- [ ] **Step 1: Write the failing tests** (SQLite; a fake `Resolver`; a local repository served as in Task 11; `t0` clock advanced by hand):
```go
func TestService_NewHeadTreeBecomesPending(t *testing.T)
func TestService_CommitOutsideSubpathNoPair(t *testing.T)
func TestService_LoadRefusalStoredFailed(t *testing.T)       // tree with nodesProvider → failed tree; FailedError names the rule
func TestService_FailedTreeErrorIsSealed(t *testing.T)       // no plaintext error in clusters row columns
func TestService_InterruptedTreeNotReprocessed(t *testing.T) // processing mark left (simulated crash) → Startup → failed "interrupted"; next poll does not reprocess; refresh retries it
func TestService_GracefulShutdownReprocesses(t *testing.T)   // ctx cancelled mid-resolve → mark cleared, last_head_tree unchanged → next Run processes the tree
func TestService_SetSameHeadTreeYieldsPending(t *testing.T)  // Set to another URL serving the same tree → pending pair created
func TestService_RefreshOrder(t *testing.T)                  // failed tree T3, good newer T4 arrives → T4 pending, failed cleared; refresh with a changed value replaces pending as (T4, new version)
func TestService_PerReferenceFailureNoBackoff(t *testing.T)  // refresh_incomplete set, attempts unchanged, no pair
func TestService_WholeCallBackoffPersists(t *testing.T)      // attempts persisted; a new Service on the same DB respects next_attempt_at
func TestService_RateLimitPausesUntilDaily(t *testing.T)
func TestService_MistypedCredentialNotRetried(t *testing.T)
func TestService_RenderUsesTicketPair(t *testing.T)          // pending pair P2 exists; ticket on approved P1 → P1's render
func TestService_RenderCacheMissNoDrift(t *testing.T)        // ErrNoRender; drift unset; render:approved unchanged
func TestService_RenderDriftSetsDrift(t *testing.T)          // stored hash altered → ErrDrift; host row drift set with drift hashes
func TestService_MoveAsideRefillsCache(t *testing.T)
func TestChecks_ClientWarnAndClear(t *testing.T)
func TestChecks_RenderApprovedKeyedOnPair(t *testing.T)      // failing pair P1, then P2 approved → check passes
```
- [ ] **Step 2: Run to verify failure**

Run: `rtk proxy go test ./internal/clusters/ -run 'TestService|TestChecks' -v`
Expected: FAIL, compile errors.

- [ ] **Step 3: Implement.** No transaction spans a fetch, a resolve or a render.

- [ ] **Step 4: Run to verify pass**

Run: `rtk proxy go test ./internal/clusters/ -race -v`
Expected: PASS; `goleak.VerifyNone` clean in every `Run` test.

- [ ] **Step 5: Gate and commit**

Run: `DOCKER_HOST=unix://$HOME/.colima/default/docker.sock rtk proxy task ci`
Expected: exit 0.
Run: `rtk proxy git add internal/clusters` then `rtk proxy git -c commit.gpgsign=false commit -m "feat: cluster revisions, secret refresh, the tree cache and drift detection" -m "Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>"`
Expected: one new commit.

---

### Task 13: Approvals, bindings and drift re-approval
Kind: rewrite

**Files:**
- Create: `internal/clusters/approve.go`, `internal/clusters/approve_test.go`, `internal/clusters/approve_integration_test.go`
- Modify: `internal/clusters/repository.go`

**Interfaces:**
- Consumes: Task 7 `fleet.TransitionTx`, `AdoptByUUIDTx`, `HostBoundTx`, `MachinesInClusterTx`, `SetTalosTupleTx`, `InsertAudit`; Task 8 `RevokeClusterTicketsTx`; Task 9 `WantTalosTx`.
- Produces (package `clusters`):
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
  func (r *Repository) EnsureAssets(ctx context.Context, m fleet.Machine, at time.Time) error   // row 8: WantTalosTx without Rearm, body from the approved host row
  func (r *Repository) Drift(ctx context.Context, m fleet.Machine) (bool, error)
  type DriftRender struct { ConfigHash, DisplayHash string; Redacted []byte }
  func (r *Repository) SetDrift(ctx context.Context, pairID, host, readHash string, d DriftRender) (bool, error)   // WHERE pair_id = ? AND host_name = ? AND config_hash = readHash
  type DriftApproveRequest struct { Cluster, Host, Hash, Actor string; At time.Time }
  func (r *Repository) ApproveDrift(ctx context.Context, req DriftApproveRequest) error   // ErrStale unless drift_config_hash == Hash; stored hashes and redacted ← drift ones; drift cleared; no ticket revoked
  type HostDiff struct { Host string; Old, New []byte; OldHash, NewHash string; SecretValuesChanged bool }
  func (r *Repository) Diff(ctx context.Context, cluster string) (pairID string, hosts []HostDiff, err error)   // pending vs approved, redacted renders opened
  func (r *Repository) DriftDiff(ctx context.Context, cluster, host string) (HostDiff, error)
  type HostRef struct{ Cluster, Host string }
  func (r *Repository) UnboundHosts(ctx context.Context) ([]HostRef, error)
  func (r *Repository) ApprovedPair(ctx context.Context, clusterID string) (Pair, error)   // ErrNotFound when the cluster has no approved pair
  ```
- `ApproveCluster`, one transaction (lock order of the Global Constraints): pending → approved; the older approved pair, its renders and version deleted; `RevokeClusterTicketsTx`; `host_left` (via `TransitionTx`) for every machine of the cluster whose state is `assigned`, `installing`, `install-unconfirmed` or `installed`, or `cordoned` over one of those, and whose host is absent from the new pair; for every remaining machine bound to the cluster, `SetTalosTupleTx` from the new host row and, when the tuple changed, `WantTalosTx{Rearm: true}` with the body; audit `cluster.approved`.
- `ApproveMachine`/`AdoptMachine`/`RebindMachine`/`AdoptByUUID`: one transaction: host in the approved pair, `HostBoundTx`, `TransitionTx` (or `AdoptByUUIDTx`) with the binding and tuple from the host row; approve and rebind into `assigned` also `WantTalosTx{Rearm: true}` with the body.

- [ ] **Step 1: Write the failing tests** (suite `runApproveSuite`; `TestApprove_SQLite`, `TestApprove_Postgres`):
```go
// ApproveClusterStale: approving a replaced pair id → ErrStale
// ApproveClusterSideEffects: revokes cluster tickets; host_left for an installed and a cordoned-over-installed machine; an orphaned machine untouched; tuple change re-arms a broken talos row and sets its body
// ApprovalDuringConfigSetsNoDrift: SetDrift with the old pair id after approval → false, nothing written
// ApproveMachineBindsAndWants: assigned; binding + tuple; talos row wanted with body
// ApproveMachineHostMissing, ApproveMachineHostBound, ApproveMachineJunk
// AdoptStoresCurrentRender: installed; no talos row created
// AdoptByUUIDFlow
// EnsureAssetsCreatesWithoutRearm: broken row stays broken; absent row created with body
// EnsureAssetsFillsMissingBody
// DriftApproveMatching, DriftApproveStale
// DiffSecretValuesChanged: same display hash, different config hash → SecretValuesChanged true
// UnboundHosts
// ApprovedPairNone, ApprovedPairCurrent
```
- [ ] **Step 2: Run to verify failure**

Run: `rtk proxy go test ./internal/clusters/ -run TestApprove -v`
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
Run: `rtk proxy git add internal/clusters` then `rtk proxy git -c commit.gpgsign=false commit -m "feat: cluster and machine approval, drift re-approval and diffs" -m "Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>"`
Expected: one new commit.

---

### Task 14: Durable store writes
Kind: rewrite

**Files:**
- Modify: `internal/store/store.go`
- Modify: `internal/store/store_test.go`
- Create: `internal/store/durable.go`, `internal/store/durable_test.go`

**Interfaces:**
- Produces (package `store`, unexported unless named):
  ```go
  func (s *Store) writeBlob(ctx context.Context, desc ocispec.Descriptor, r io.Reader) error   // under os.Root: ingest/<random>, Sync, verify size and digest, rename to blobs/sha256/<hex>, fsync blobs/sha256/
  func (s *Store) syncDir(rel string) error   // EINVAL/ENOTSUP → logged once at Warn through the store's error handler, nil; other errors returned
  func WithErrorHandler(fn func(context.Context, error)) Option   // new Open option (Open gains variadic opts)
  ```
- Changes: `PushBlob` writes through `writeBlob` instead of `oci.Push`; a blob present at the right size is re-hashed against its digest: equal → `Sync` the file and fsync `blobs/sha256/`, return; different → write the new blob to `ingest/` and rename over it (never remove first). `TagArtifact` packs the manifest in memory (`oras.PackManifest` against an in-memory target), writes the empty config blob and the manifest through `writeBlob`, then calls `oci.Tag`. `Open` creates `ingest/` and `blobs/sha256/` when missing, then fsyncs `blobs/sha256/`, `blobs/` and the root.

- [ ] **Step 1: Write the failing tests** (a test seam swaps the file-sync and dir-sync functions):
```go
func TestPushBlob_SyncsBeforeReturn(t *testing.T)          // recorded order: file Sync, rename, dir sync of blobs/sha256
func TestPushBlob_SyncErrorFails(t *testing.T)             // injected file Sync error → PushBlob error; no blob at the final path
func TestPushBlob_PresentWrongHashReplaced(t *testing.T)   // right-size zero-filled file at the digest path → replaced via rename; content now matches
func TestPushBlob_PresentRightHashSynced(t *testing.T)
func TestTagArtifact_ManifestDurableBeforeTag(t *testing.T) // manifest and config blobs synced before index.json names the tag
func TestOpen_FreshStoreCreatesDirs(t *testing.T)          // empty root → Open ok; ingest/ and blobs/sha256/ exist
func TestSyncDir_EINVALTolerated(t *testing.T)             // injected EINVAL → nil and one error-handler report; EIO → error
func TestStore_ConcurrentPushSameDigest(t *testing.T)      // existing behaviour still holds
```
- [ ] **Step 2: Run to verify failure**

Run: `rtk proxy go test ./internal/store/ -run 'TestPushBlob|TestTagArtifact|TestOpen_Fresh|TestSyncDir' -v`
Expected: FAIL, compile errors.

- [ ] **Step 3: Implement.**

- [ ] **Step 4: Run to verify pass**

Run: `rtk proxy go test ./internal/store/ ./internal/toolcache/ ./internal/boot/ -race -v`
Expected: PASS.

- [ ] **Step 5: Gate and commit**

Run: `DOCKER_HOST=unix://$HOME/.colima/default/docker.sock rtk proxy task ci`
Expected: exit 0.
Run: `rtk proxy git add internal/store` then `rtk proxy git -c commit.gpgsign=false commit -m "fix: make every blob a tag names durable before the tag" -m "Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>"`
Expected: one new commit.

---

### Task 15: Fetch-source seam (behaviour-preserving)
Kind: refactor

**Files:**
- Modify: `internal/toolcache/fetcher.go`
- Create: `internal/toolcache/source.go`, `internal/toolcache/utility_source.go`

**Interfaces:**
- Produces (package `toolcache`, unexported):
  ```go
  type claimClass int
  const ( classUrgent claimClass = iota; classAssigned; classRest; classOther )
  type source interface {
      claim(ctx context.Context, class claimClass, at time.Time) (job, bool, error)
  }
  type job interface {
      name() string                                // "<entry>/<arch>" for utilities
      run(ctx context.Context) error
      record(ctx context.Context, o outcome) error // settles the row: ready, deferred, failure
      discard(ctx context.Context)
      close(ctx context.Context)
  }
  type utilitySource struct{ f *Fetcher }
  ```
- The worker asks `f.sources` (today only `utilitySource`) for each class in order `classUrgent, classAssigned, classRest, classOther`; `utilitySource.claim` returns the result of today's `Repo.ClaimFetch` for `classUrgent` and nothing for the other classes, which reproduces today's order exactly. The utility job is today's `fetchJob` behind `job`. `FetchEvent`, metric names, log lines and every exported symbol are unchanged.

- [ ] **Step 1: Run the existing tests as the baseline.**

Run: `rtk proxy go test ./internal/toolcache/ -race -count=1 -v`
Expected: PASS (record the test count).

- [ ] **Step 2: Refactor** to the types above. No test file changes except where a test reaches an unexported name that moved.

- [ ] **Step 3: Run to verify behaviour is preserved**

Run: `rtk proxy go test ./internal/toolcache/ -race -count=1 -v`
Expected: PASS with the same test count as Step 1.

- [ ] **Step 4: Gate and commit**

Run: `DOCKER_HOST=unix://$HOME/.colima/default/docker.sock rtk proxy task ci`
Expected: exit 0.
Run: `rtk proxy git add internal/toolcache` then `rtk proxy git -c commit.gpgsign=false commit -m "refactor: put the utility fetch job behind a source seam" -m "Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>"`
Expected: one new commit.

---

### Task 16: Resume only on a matching ETag; size from the response
Kind: rewrite

**Files:**
- Modify: `internal/toolcache/fetcher.go`
- Modify: `internal/toolcache/fetcher_test.go`

**Interfaces:**
- Changes: `accept` on a 206 requires the response's strong `ETag` to equal the sidecar's; otherwise the spool is reset and the download restarts from zero in the same attempt (one restart per attempt; a second mismatch is an ordinary error). Shared helper `func responseSize(resp *http.Response, have int64) (int64, error)`: on 200 `Content-Length` (must be ≥ 0), on 206 the total of `Content-Range`; it is used by sources without a pinned size (Task 17).

- [ ] **Step 1: Write the failing tests**
```go
func TestFetcher_206WithOtherETagRestarts(t *testing.T)   // upstream: first response cut at half with ETag "A"; on resume it answers 206 with ETag "B" despite If-Range "A" → the spool is reset, a full 200 is fetched, sha matches the pin
func TestFetcher_206WithSameETagResumes(t *testing.T)
func TestResponseSize(t *testing.T)                       // 200 CL 10 → 10; 206 "bytes 5-9/10" → 10; 200 CL -1 → error; 206 bad range → error
```
- [ ] **Step 2: Run to verify failure**

Run: `rtk proxy go test ./internal/toolcache/ -run 'TestFetcher_206|TestResponseSize' -v`
Expected: FAIL (the first test fails on the spliced hash; `responseSize` undefined).

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

### Task 17: The factory client and the Talos source
Kind: rewrite

**Files:**
- Create: `internal/factory/doc.go`, `internal/factory/base.go`, `internal/factory/client.go`, `internal/factory/client_test.go`, `internal/toolcache/talos_source.go`, `internal/toolcache/talos_source_test.go`, `internal/toolcache/talos_reconcile.go`, `internal/toolcache/talos_reconcile_test.go`
- Modify: `internal/toolcache/fetcher.go`
- Modify: `internal/toolcache/reconcile.go`
- Modify: `internal/fleet/utility_repository.go`

**Interfaces:**
- Produces (package `factory`):
  ```go
  const Base = "https://factory.talos.dev"          // base.go holds only this (the e2e overlay replaces the file)
  type Asset string
  const ( AssetKernel Asset = "kernel"; AssetInitramfs Asset = "initramfs"; AssetCmdline Asset = "cmdline" )
  func FileName(a Asset, arch fleet.Arch) string    // "kernel-amd64", "initramfs-amd64.xz", "cmdline-metal-amd64"
  type Client struct{ /* base string, *http.Client */ }
  func New(base string, hc *http.Client) *Client
  func (c *Client) URL(schematic, version string, arch fleet.Arch, a Asset) string   // <base>/image/<schematic>/<version>/<file>
  func (c *Client) Register(ctx context.Context, body []byte) (string, error)         // POST <base>/schematics, Content-Type application/yaml; 200/201 JSON {"id": "..."}; other statuses an error carrying the printable body (≤ 1 KiB)
  ```
- Produces (package `fleet`): `func (r *Repository) ClaimFetchAtPriority(ctx context.Context, priority int, at time.Time) (CacheRow, bool, error)` (only rows at that priority).
- Produces (package `toolcache`): `FetcherConfig` gains `Talos *factory.Client` (nil disables the Talos source); `FetchEvent` gains `Source string` (`"utility"` or `"talos"`) and `Talos fleet.TalosKey`; `utilitySource.claim` serves `classUrgent` with priority 1 and `classRest` with priority 0; `talosSource.claim` serves `classAssigned` (`fleet.TalosAssigned`) and `classOther` (`fleet.TalosOther`). `func ReconcileTalos(ctx context.Context, cfg ReconcileConfig) (TalosReport, error)`; `type TalosReport struct{ Interrupted, Requeued, Restored int }`; `func TalosSpoolName(key fleet.TalosKey, n int) string` = `"talos-" + key.Schematic[:12] + "-" + key.Version + "-" + string(key.Arch) + "-" + strconv.Itoa(n)`; `TalosTag(key) string` = `"talos/" + key.Schematic + "-" + string(key.Arch) + ":" + key.Version`; `const TalosArtifactType = "application/vnd.hephaestus.talos.v1"`.
- Talos job (spec §5.7 steps 1–7): register when the row has a body (2-minute deadline; returned id ≠ key → integrity); fetch the three assets in the order kernel, initramfs, cmdline through the shared download helpers with `responseSize` and a 1 GiB cap, reserving space after the headers; a 404 → integrity class with the factory body as cause; on a first fetch record each asset's size and sha256, on a later fetch compare (mismatch → integrity); validate the cmdline (printable ASCII, no `$`, no newline, no token starting `#`, no `;`, `&&`, `||`, ≤ 2046 bytes); push each asset as a layer titled by its asset name; tag `TalosTag`; the shortcut (`CheckLayers` passes and layer digests equal the recorded hashes) applies only when the row has recorded assets. Metrics `hephaestus.talos.fetch.bytes` and `hephaestus.talos.fetch.failures{class}`.
- Reconcile: `ReconcileTalos` after the utility reconcile: `ResetTalosFetching`; `ready` rows whose tag is lost or whose layer digests differ from the recorded hashes → `RequeueTalos`; `broken` stays; a `wanted`/`failed` row with recorded assets whose tag passes → ready. `sweepSpool` keeps `TalosSpoolName(key, n)` and its `.etag` for every row not `ready`.

- [ ] **Step 1: Write the failing tests** (a fake factory `httptest` TLS 1.3 server behaving as the real one: `/image/…` answers 302 to `/assets/<id>?expires=<unix>`; an expired URL answers 403; ranges honoured but `If-Range` ignored; strong ETags; `POST /schematics` returns `{"id": …}`):
```go
func TestFactory_URLAndFileNames(t *testing.T)                 // amd64: kernel-amd64, initramfs-amd64.xz, cmdline-metal-amd64; arm64 likewise
func TestFactory_Register(t *testing.T)                        // returns id; a 400 returns an error carrying the body
func TestTalosSource_FirstFetchRecordsAssets(t *testing.T)      // row ready; Assets sizes and sha256 equal the served bytes; tag present
func TestTalosSource_FirstFetchSizedFromContentRange(t *testing.T) // interrupted first fetch resumed with 206 → size from the Content-Range total
func TestTalosSource_ExpiredURLRestartsAtFactory(t *testing.T)
func TestTalosSource_RecordedHashMismatchIsIntegrity(t *testing.T) // RequeueTalos then serve different bytes → broken
func TestTalosSource_UnknownSchematicBroken(t *testing.T)       // 404 → broken at once; LastError holds the factory body
func TestTalosSource_RegisterIDMismatchBroken(t *testing.T)
func TestTalosSource_CmdlineRejected(t *testing.T)              // table: "#x", "a;b", "a && b", "a$b", "a\nb", 2047 bytes → broken
func TestTalosSource_ShortcutNeedsRecordedHashes(t *testing.T)
func TestFetcher_ClaimOrderAcrossSources(t *testing.T)          // utility p1, talos assigned, utility p0, talos other → claimed in that order
func TestReconcileTalos(t *testing.T)                           // fetching→wanted; lost ready→wanted; digest mismatch→wanted; broken stays
func TestSweepSpool_KeepsTalosSpools(t *testing.T)
```
- [ ] **Step 2: Run to verify failure**

Run: `rtk proxy go test ./internal/factory/ ./internal/toolcache/ -run 'TestFactory|TestTalos|TestFetcher_ClaimOrderAcrossSources|TestReconcileTalos|TestSweepSpool_KeepsTalosSpools' -v`
Expected: FAIL, compile errors.

- [ ] **Step 3: Implement.**

- [ ] **Step 4: Run to verify pass**

Run: `rtk proxy go test ./internal/factory/ ./internal/toolcache/ ./internal/fleet/ -race -v`
Expected: PASS.
Run: `DOCKER_HOST=unix://$HOME/.colima/default/docker.sock rtk proxy go test -tags integration ./internal/fleet/ -race -run Postgres -v`
Expected: PASS.

- [ ] **Step 5: Gate and commit**

Run: `DOCKER_HOST=unix://$HOME/.colima/default/docker.sock rtk proxy task ci`
Expected: exit 0; no `not executed:` line.
Run: `rtk proxy git add internal/factory internal/toolcache internal/fleet` then `rtk proxy git -c commit.gpgsign=false commit -m "feat: prefetch Talos boot assets from the image factory" -m "Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>"`
Expected: one new commit.

---

### Task 18: Boot rows 8–10, tickets, the install script and the Talos artifact route
Kind: rewrite

**Files:**
- Modify: `internal/boot/handlers.go`
- Modify: `internal/boot/decision.go`
- Modify: `internal/boot/scripts.go`
- Modify: `internal/boot/handlers_test.go`
- Modify: `internal/boot/decision_test.go`
- Create: `internal/boot/install.go`, `internal/boot/install_test.go`, `internal/boot/talos_artifact.go`, `internal/boot/talos_artifact_test.go`

**Interfaces:**
- Consumes: Tasks 8, 9, 12, 13, 17 (`clusters.ErrNoRender`, `clusters.ErrDrift`, `factory.Asset`, `toolcache.TalosTag`).
- Produces (package `boot`):
  ```go
  type ApprovedPair struct { PairID, SecretVersionID string }
  type ConfigSource interface {                                            // *clusters.Service implements it; tests use a double
      Drift(ctx context.Context, m fleet.Machine) (bool, error)
      EnsureAssets(ctx context.Context, m fleet.Machine, at time.Time) error
      Approved(ctx context.Context, clusterID string) (ApprovedPair, error)
      Render(ctx context.Context, t fleet.Ticket) ([]byte, error)
  }
  // Config gains: Configs ConfigSource (nil keeps M2 behaviour: rows 8–10 hold)
  const MaxCommandLine = 2046
  var ErrCommandLineTooLong = errors.New("boot: kernel command line too long")
  type InstallRequest struct { Address netip.Addr; FQDN, Schematic, Version string; Arch fleet.Arch; Cmdline, Token string }
  func InstallScript(req InstallRequest) (string, error)
  ```
  `*clusters.Service` gains `Drift`, `EnsureAssets` and `Approved` (from `Repository.ApprovedPair`), delegating to its `Repository` (Task 13 methods).
- The install script, exactly:
  ```
  #!ipxe
  imgfree
  kernel --name k http://<address>/t/<schematic>/<version>/<arch>/kernel <cmdline> talos.halt_if_installed=1 talos.config=https://<fqdn>/config?u=${uuid}&t=<token>
  initrd http://<address>/t/<schematic>/<version>/<arch>/initramfs
  boot
  ```
  Its length check counts `"k " + <cmdline> + " talos.halt_if_installed=1 talos.config=https://<fqdn>/config?u=" + 36 + "&t=" + 43`, against `MaxCommandLine`.
- `Decide`: row 2's mismatch test is skipped when `!in.Machine.IdentityObserved`. `decideAndRecord` sets `Input.AssetsReady` (the machine's `talos_cache` row is `ready`, its tag passes `CheckLayers`, and its layer digests equal the recorded hashes; a lost layer → `RequeueTalos`; called only for an `assigned` machine with a tuple) and `Input.Drift` (`Configs.Drift`); a not-ready assigned machine calls `Configs.EnsureAssets`. `fallback` and `reasonNoTicket` are removed. After `RecordBoot`, a row-10 install mints a ticket (`fleet.MintTicket` with `Configs.Approved`), reads the cached cmdline blob, and serves `InstallScript`; `ErrCommandLineTooLong` serves a hold with reason `kernel command line too long` and counts alert kind `cmdline_too_long`; any other mint or render failure is a 500. Every minted ticket increments `hephaestus.tickets.minted`.
- `/t/{schematic}/{version}/{arch}/{asset}` (asset `kernel` or `initramfs`) on `PlainHTTP()` and `TLS()`: resolves `TalosTag`, finds the layer titled by the asset, checks size and the recorded sha256, serves with `http.ServeContent` through `idleWriter`; lost content → `RequeueTalos` and 404; anything unknown → 404.

- [ ] **Step 1: Write the failing tests**
```go
func TestDecide_UnobservedSkipsRow2(t *testing.T)
func TestInstallScript_Exact(t *testing.T)                // the five lines above for amd64 fixture values
func TestInstallScript_LengthBoundary(t *testing.T)      // cmdline making the count exactly 2046 → ok; 2047 → ErrCommandLineTooLong
func TestBoot_AssignedNotReadyHoldsRow8AndEnsures(t *testing.T)   // ConfigSource double records EnsureAssets once
func TestBoot_AssignedDriftHoldsRow9(t *testing.T)
func TestBoot_AssignedReadyServesInstallScript(t *testing.T)      // row 10; script contains the minted token; a tickets row exists with that hash
func TestBoot_CmdlineTooLongHolds(t *testing.T)
func TestBoot_ReadyRowLostLayerRequeues(t *testing.T)
func TestBoot_NoConfigSourceHolds(t *testing.T)           // Configs nil → assigned machine holds at row 8 (M2 behaviour)
func TestTalosArtifact_ServesKernelWithRange(t *testing.T)
func TestTalosArtifact_UnknownAsset404(t *testing.T)
func TestTalosArtifact_LostLayerRequeues404(t *testing.T)
```
- [ ] **Step 2: Run to verify failure**

Run: `rtk proxy go test ./internal/boot/ -run 'TestDecide_Unobserved|TestInstallScript|TestBoot_|TestTalosArtifact' -v`
Expected: FAIL, compile errors.

- [ ] **Step 3: Implement**; remove the `fallback` test rows that assert the M1 hold.

- [ ] **Step 4: Run to verify pass**

Run: `rtk proxy go test ./internal/boot/ ./internal/clusters/ -race -v`
Expected: PASS.

- [ ] **Step 5: Gate and commit**

Run: `DOCKER_HOST=unix://$HOME/.colima/default/docker.sock rtk proxy task ci`
Expected: exit 0.
Run: `rtk proxy git add internal/boot internal/clusters` then `rtk proxy git -c commit.gpgsign=false commit -m "feat: install script with a single-use ticket and the Talos artifact route" -m "Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>"`
Expected: one new commit.

---

### Task 19: The /config route
Kind: rewrite

**Files:**
- Modify: `internal/boot/handlers.go`
- Create: `internal/boot/config.go`, `internal/boot/config_test.go`

**Interfaces:**
- Produces (package `boot`): `func (h *Handlers) Config() http.Handler` — `GET /config` behind its own subnet guard that answers 404; `TLS()` and `Proxied()` route `/config` to it outside `RequireSubnet` (in proxy mode `TrustProxies` still wraps it, so its 403s stand); `func (h *Handlers) RunRecorder(ctx context.Context) error` — runs the background recording retries until ctx ends, then logs abandoned ones through the error handler. Metric `hephaestus.config.requests{outcome}` (`served`, `refused_<reason>`) and `hephaestus.config.record.failures`.
- Order (spec §5.8): TLS; subnet (404); `u` and `t` present; `TicketByToken` (unknown → 404); `t.Valid(now)` and the machine's UUID equals `u`; `MarkTicketRequested`; machine `assigned`, not junk, bound to the ticket's host; ticket pair and version equal `Configs.Approved`; `Configs.Render` (`ErrNoRender` → 404; `ErrDrift` → 404); write with `Content-Type: application/yaml`, `Cache-Control: no-store`, flush via `http.ResponseController`; `RecordConfigServed`; when the ticket's pair is not the current approved pair, audit `config.older_pair` with both ids. A recording error → count, log, and enqueue the ticket id in the recorder (one entry per ticket id); the recorder retries with backoff 1 s doubling to 1 h while the machine stays `assigned` and bound to that host; `AlreadyConsumed` or the machine `installing` → drop silently; still failing at 1 h → alert `config_unrecorded` naming the machine and its state. Every refusal is a 404 audited at most once per hour per host, or per reason before a host is known, and never consumes the ticket. The wide event never carries the ticket.

- [ ] **Step 1: Write the failing tests**
```go
func TestConfig_Served(t *testing.T)                // 200; body == render; Cache-Control no-store; ticket consumed; machine installing
func TestConfig_Refusals(t *testing.T)              // table, each 404 and ticket unconsumed: plain HTTP (no route), off-subnet, missing t, forged t, expired, reused (consumed), revoked, other UUID, unassigned machine, stale pair, junk machine, drift, cache miss
func TestConfig_OffSubnetIs404Not403(t *testing.T)
func TestConfig_RendersTicketPair(t *testing.T)     // a pending pair exists; Render called with the ticket's pair
func TestConfig_ApprovalRaceRecordsOlderPair(t *testing.T) // approval between check and record → recorded; audit config.older_pair
func TestConfig_RecordingRetryStopsOnConsumed(t *testing.T)
func TestConfig_RecordingRetryStopsOnInstalling(t *testing.T)
func TestConfig_RecordingRetryOnePerTicket(t *testing.T)
func TestConfig_RecordingAbandonedAtShutdownLogged(t *testing.T)
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

### Task 20: Serve wiring: default transport, clusters, Talos and checks
Kind: rewrite

**Files:**
- Modify: `cmd/hephaestus/serve.go`
- Modify: `cmd/hephaestus/serve_test.go`
- Create: `cmd/hephaestus/transport.go`, `cmd/hephaestus/transport_test.go`

**Interfaces:**
- Produces (package `main`): `func setupDefaultTransport(counter *secrets.Counter)` — mutates the original `*http.Transport` behind `http.DefaultTransport` in place (`TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS13}`, dial timeout 10 s via a `net.Dialer`, `TLSHandshakeTimeout` 10 s, `ResponseHeaderTimeout` 60 s) and sets `http.DefaultClient.Transport = counter` wrapping it (`//nolint:forbidigo // the 1Password SDK sends through http.DefaultClient`); idempotent; called first in `listenAndServe`, before `toolcache.NewHTTPClient`.
- `runServe` order: `db.Migrate` → `datakey.EnsureCanary` (failure stops serve, naming `HEPHAESTUS_DATA_KEY`) → `store.Open` → utility reconcile and GC → `toolcache.ReconcileTalos` → `clusters.NewRepository`, `clusters.NewService` (`Resolver` = `secrets.NewOnePassword(cfg.OPToken, buildVersion())` only when the token is set) → `svc.Startup` → `NewFetcher` with `Talos: factory.New(factory.Base, deps.client)` → checks (`svc.Checks()` added to the registry; `talos:cache` warn on any broken row; `config:pending` warn listing tickets live and unfetched for 30 min with the two-branch runbook text) → `boot.NewHandlers` with `Configs: svc` → listeners. `work` stage gains `clusters` (`svc.Run`), `config recorder` (`handlers.RunRecorder`) and `ticket pruning` (hourly `PruneTickets(now - 7 days)`). Fetch events log `source`; Talos events log the tuple. An `Int64ObservableGauge` `hephaestus.secrets.requests` reports `counter.Last24h()`. `serve --help` lists `HEPHAESTUS_DATA_KEY`, `HEPHAESTUS_DATA_KEY_FILE`, `HEPHAESTUS_OP_TOKEN`, `HEPHAESTUS_OP_TOKEN_FILE`.

- [ ] **Step 1: Write the failing tests**
```go
func TestSetupDefaultTransport_FloorReachesClients(t *testing.T) // after setup: http.DefaultTransport.(*http.Transport).TLSClientConfig.MinVersion == TLS13; a TLS 1.2-only httptest server → http.DefaultClient errors; go-git's githttp.DefaultClient errors against it too
func TestSetupDefaultTransport_CountsOnePassword(t *testing.T)    // a request to a test host mapped as my.1password.com → counter 1
func TestSetupDefaultTransport_ToolcacheClientStillBuilds(t *testing.T) // toolcache.NewHTTPClient does not panic after setup
func TestRunServe_WrongDataKeyStops(t *testing.T)                 // canary written with key A; serve with key B → error naming HEPHAESTUS_DATA_KEY
func TestRunServe_StartsWithoutOPToken(t *testing.T)              // ready; secrets:token warn only when a cluster exists
func TestRunServe_ShutdownOrderClearsProcessing(t *testing.T)     // a processing mark set by the service is cleared at shutdown; goleak clean
```
- [ ] **Step 2: Run to verify failure**

Run: `rtk proxy go test ./cmd/hephaestus/ -run 'TestSetupDefaultTransport|TestRunServe_WrongDataKey|TestRunServe_StartsWithoutOPToken|TestRunServe_ShutdownOrderClearsProcessing' -v`
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

### Task 21: CLI: cluster and audit commands
Kind: rewrite

**Files:**
- Modify: `cmd/hephaestus/cli.go`
- Modify: `cmd/hephaestus/main.go`
- Modify: `cmd/hephaestus/cli_test.go`
- Create: `cmd/hephaestus/clustercmd.go`, `cmd/hephaestus/clustercmd_test.go`, `cmd/hephaestus/auditcmd.go`, `cmd/hephaestus/auditcmd_test.go`

**Interfaces:**
- Produces (package `main`): `func openCLIStores(cmd *cobra.Command, deps cliDeps) (cliStores, func() error, error)` with `type cliStores struct { Fleet *fleet.Repository; Clusters *clusters.Repository; Catalog *catalog.Catalog }` (database and data key only; never the content store or the git store); `newClusterCmd(deps)` with `import`, `set`, `remove`, `list`, `diff` (`--host`), `approve` (`<name> <pair id>` or `--host <host> --hash <hash>`), `refresh`; `newAuditCmd(deps)` with `list [--since <duration>] [--machine <mac|uuid>]`. Every command takes `--json`; text output uses `writeTable`/`safeText`; JSON uses `fleet.PrintableText` for stored text. `fleet.Repository` gains `func (r *Repository) AuditEvents(ctx context.Context, since time.Time, machineID string) ([]AuditRecord, error)` (`AuditRecord{At, Actor, Action, MachineID, Detail string}`). `cluster diff` prints a unified diff of the redacted renders per host and "secret values changed" where it applies.

- [ ] **Step 1: Write the failing tests** (CLI against `dbtest.SQLitePair`, as M2's `cli_test.go` does):
```go
func TestClusterImport_JSON(t *testing.T)          // writes the row; prints {"name":"lab",…}
func TestClusterImport_RefusesUserinfo(t *testing.T)
func TestClusterSet_ChangedRef(t *testing.T)
func TestClusterRemove_InUse(t *testing.T)
func TestClusterDiff_PendingVsApproved(t *testing.T) // never prints a secret value
func TestClusterApprove_Stale(t *testing.T)
func TestClusterApprove_DriftHash(t *testing.T)
func TestClusterRefresh_SetsFlag(t *testing.T)
func TestAuditList_SinceAndMachine(t *testing.T)
func TestCLI_NeverOpensStores(t *testing.T)        // a data dir without store/ or git/ → every cluster command works
```
- [ ] **Step 2: Run to verify failure**

Run: `rtk proxy go test ./cmd/hephaestus/ -run 'TestCluster|TestAuditList|TestCLI_NeverOpensStores' -v`
Expected: FAIL, compile errors.

- [ ] **Step 3: Implement.**

- [ ] **Step 4: Run to verify pass**

Run: `rtk proxy go test ./cmd/hephaestus/ ./internal/fleet/ -race -v`
Expected: PASS.

- [ ] **Step 5: Gate and commit**

Run: `DOCKER_HOST=unix://$HOME/.colima/default/docker.sock rtk proxy task ci`
Expected: exit 0; no `not executed:` line.
Run: `rtk proxy git add cmd/hephaestus internal/fleet` then `rtk proxy git -c commit.gpgsign=false commit -m "feat: cluster and audit admin commands" -m "Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>"`
Expected: one new commit.

---

### Task 22: CLI: machine and unknown-action commands
Kind: rewrite

**Files:**
- Modify: `cmd/hephaestus/main.go`
- Create: `cmd/hephaestus/machinecmd.go`, `cmd/hephaestus/machinecmd_test.go`, `cmd/hephaestus/unknowncmd.go`, `cmd/hephaestus/unknowncmd_test.go`

**Interfaces:**
- Produces (package `main`): `newMachineCmd(deps)` with `list`, `show <mac|uuid>` (state, binding, tuple, Talos row state, live ticket minted time and whether `/config` was requested), `fetch <mac|uuid> [--reset-hash]`, `approve <mac|uuid> <cluster>/<host>`, `adopt <mac|uuid> <cluster>/<host>` and `adopt --uuid <uuid> --arch <amd64|arm64> <cluster>/<host>`, `mark-installed`, `abort`, `reinstall --topf-reset-full-done` (refused without the flag; help text gives the runbook order: `topf reset` (default `--full=true`), then this command, then power-cycle; it warns that `--full=false` keeps META and halts), `unassign`, `unbind`, `rebind <cluster>/<host>`, `cordon`, `uncordon`, `local-only`, `unmark-local-only`, `reject`, `unreject`, `forget`; `abort`'s help warns that aborting after the installer wrote META needs a full reset. `newUnknownActionCmd(deps)` with `set <hold|local>`: `hold` lists `clusters.UnboundHosts` and `discovered` machines seen in the last 7 days and requires `--yes`. Machine refs resolve through `resolveMachine`; binding commands go through `clusters.Repository`, the rest through `fleet.Repository.Transition` with actor `fleet.ActorCLI`.

- [ ] **Step 1: Write the failing tests**
```go
func TestMachineApprove(t *testing.T)               // assigned; binding shown in --json
func TestMachineAdoptByUUID(t *testing.T)
func TestMachineReinstallNeedsFlag(t *testing.T)    // without --topf-reset-full-done → usage error; with it → assigned, Talos row re-armed
func TestMachineTransitions(t *testing.T)           // table over every remaining command: from-state → to-state, audit action
func TestMachineFetchResetHash(t *testing.T)
func TestMachineShow_PendingTicket(t *testing.T)
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

### Task 23: End-to-end harness for approval and the install path
Kind: rewrite

**Files:**
- Modify: `test/e2e/run.sh`
- Modify: `test/e2e/gen-certs.sh`
- Modify: `test/e2e/README.md`
- Create: `test/e2e/upstreams/main.go`, `test/e2e/upstreams/main_test.go`, `test/e2e/overlay/base.go.in`, `test/e2e/overlay/resolver.go.in`, `test/e2e/fixture-cluster/` (a Topf tree `lab` with the plain schematic and fixture references, plus `values.json`)

**Interfaces:**
- `test/e2e/upstreams/main.go` (package main, stdlib only): one TLS 1.3 server for `factory.e2e.test` (the fake factory of Task 17: 302 to expiring asset URLs, `If-Range` ignored, `POST /schematics`, small fake kernel/initramfs/cmdline files) and `git.e2e.test` (smart HTTP via `net/http/cgi` + `git http-backend` over a repository built from `test/e2e/fixture-cluster/` at start). It runs in a `golang:1.27.4-trixie@sha256:<digest>` container at a new fixed address `10.99.0.13`.
- `gen-certs.sh` also issues `factory.e2e.test` and `git.e2e.test` leaves.
- Overlays (copied into the temporary tree like `catalog.go.in`): `base.go.in` → `internal/factory/base.go` (`const Base = "https://factory.e2e.test"`); `resolver.go.in` → `internal/secrets/onepassword.go` with the same exported API, resolving from the fixture `values.json` embedded at build time; the harness sets `HEPHAESTUS_OP_TOKEN=e2e-dummy` and `HEPHAESTUS_DATA_KEY` to the fixture key.
- New flow after the M2 utility phase: `heph cluster import lab --repo https://git.e2e.test/lab.git --ref main --path lab`; wait for a pending pair (`heph cluster list --json`); `heph cluster approve lab <pair id>`; `heph machine approve 52:54:00:e2:e0:03 lab/cp-01`; boot `ovmf`; checks: the Talos row reaches `ready`; `/boot` answers row 10 with an install script; the server log shows `GET /t/<schematic>/v1.12.11/amd64/kernel` and `/initramfs`; `heph machine show` reports a live ticket. Replace `seed_hold` with `heph unknown-action set hold --yes`.
- The unattended run does not execute `task e2e` (run contract); the owner runs it.

- [ ] **Step 1: Write the failing test** for the upstreams program:
```go
func TestUpstreams_FactoryRedirectAndRange(t *testing.T) // 302 → asset; Range honoured; mismatched If-Range still 206
func TestUpstreams_RegisterSchematic(t *testing.T)
func TestUpstreams_GitSmartHTTP(t *testing.T)            // go-git clones the fixture repository through it
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
Run: `rtk proxy sh -c "cp test/e2e/overlay/resolver.go.in build/resolver_check.go && go vet build/resolver_check.go; rm -f build/resolver_check.go"`
Expected: no output other than vet results for that file, exit 0. (If `go vet` cannot check the file outside its package, instead copy the tree with `git ls-files -z | xargs -0 tar -cf - | tar -xf - -C build/e2e-check`, apply both overlays, and run `go build ./...` there; Expected: exit 0.)

- [ ] **Step 5: Gate and commit**

Run: `DOCKER_HOST=unix://$HOME/.colima/default/docker.sock rtk proxy task ci`
Expected: exit 0.
Run: `rtk proxy git add test/e2e` then `rtk proxy git -c commit.gpgsign=false commit -m "test: end-to-end approval, install script and Talos asset fetch" -m "Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>"`
Expected: one new commit.
