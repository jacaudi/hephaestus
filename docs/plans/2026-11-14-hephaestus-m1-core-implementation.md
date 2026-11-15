# hephaestus M1 core Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build the hephaestus skeleton, the owner's standards (config, logs, admin listener, DB, lint, image) and the boot plane: proxyDHCP, TFTP, embedded iPXE, `/start`, `/boot` (the full §5.2 decision), `/discover`, and Let's Encrypt TLS.

**Architecture:** One Go binary (`cmd/hephaestus serve`) with concrete packages under `internal/`: `fleet` (identity, lifecycle, persistence), `boot` (decision, scripts, HTTP routes, embedded iPXE), vendored `third_party/omniprovider` (proxyDHCP, TFTP), `tlsacme`, `db`, `admin`, `telemetry`, `datakey`, `secret`. SQLite or Postgres through bun and goose. Boot-facing listeners bind the provisioning VLAN IP; the admin listener binds the management address.

**Tech Stack:** Go 1.26 language level, toolchain go1.27.4 (`toolchain` line; host `GOTOOLCHAIN=auto` fetches it), Cobra, Viper, log/slog, OpenTelemetry + Prometheus exporter, bun, goose v3, modernc SQLite, pgx v5, insomniacslk/dhcp, pin/tftp, go-acme/lego v4, iPXE `9261ebe`, Task, Docker (colima).

**Spec:** docs/designs/2026-11-13-hephaestus-core-design.md

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
- Integration: (a) merge locally into main after the full suite passes; remove the worktree after merge.
- Base branch: main.
- Remote and forge: none (local-only repo; no origin). Option (b) not available.
- Repository owned by the user: yes (local, destined for jacaudi on Forgejo).
- PR/MR details: not applicable.
- Direct push to the base branch: never.
- Branch name: feat/hephaestus-m1-core; worktree at .worktrees/feat/hephaestus-m1-core.
- Sanctioned test command: `go test ./...` until Task 1 creates taskfile.yml, then `task ci`.
- Sanctioned environment: host toolchain (no .devcontainer). Postgres tests via Docker on colima with DOCKER_HOST=unix://$HOME/.colima/default/docker.sock. `task e2e` excluded from this run (Linux-only).
- New dependencies: allowed only with a present requirement, pinned, named in the report.
- Boundaries: 8 hours wall clock; dispatch cap 6 × tasks + 12 + A/B files; 2 hours without a task completing.
- Worktree after (a) merge: remove it.
- Signing: every commit signed via the 1Password SSH agent; a signing failure pauses the run; never --no-gpg-sign.
- Milestones: this plan is M1 of 4 (M2 store/catalog/mirror, M3 clusters/render/secrets/config, M4 auth/UI/ops); auth moved from M1 to M4 with its first consumer, the UI (controller ruling).

## Header notes: M1 scope and rulings

**In M1:** repo skeleton and standards; `hephaestus serve` and `version`; config; logs and wide events; admin listener with a check registry; DB with two migration sets; fleet identity, the full §5.2 transition table and the full `/boot` decision; `/start`, `/boot`, `/discover`; the source-subnet guard; vendored proxyDHCP and TFTP; the iPXE build with committed binaries; Let's Encrypt via DNS-01 on Cloudflare; the container image.

**A/B files:** 0. Every task that modifies an existing file is `Kind: rewrite` (see the go.mod ruling below), so no task is `Kind: refactor`.

**Known limitations (M1):**
- EFI iPXE reports `${memsize}` as 0 (EFI builds use `MEMMAP_NULL`); hephaestus stores and displays `unknown` for 0. `MEMMAP_SETTINGS` stays enabled for BIOS.
- CPUID facts (`${cpuvendor}`, `${cpumodel}`) may be absent on BIOS builds (`CPUID_SETTINGS` is not enabled there, design §5.10 "where available").

**Not in M1:** auth and UI (M4); store, catalog, mirror, prefetch (M2); clusters, render, secrets, `/config`, tickets (M3); backup and recover (M4); the `/start`-without-`/boot` alert evaluator and the menu-preview endpoint (M4, with the UI and the other alert surfaces).

**Controller rulings this plan relies on:**
1. M1 = skeleton + standards + boot plane; auth moves to M4.
2. Lint falls back to stock golangci-lint with go-tricorder's `config/golangci.yml` minus the `tricorder` linter and its `settings.custom` block, because the tricorder plugin cannot resolve (verified 2026-11-14: `go list -m github.com/jacaudi/go-tricorder` fails, private repo). depguard keeps `spf13/viper` and `spf13/cobra` confined to `cmd/**`.
3. Decision inputs owned by later milestones (`assets_ready`, `drift`, utility readiness, boot-next, host binding, ticket) exist as inputs. In M1 the `/boot` caller turns `needs_ticket` into a hold with reason `ticket minting not available (M3)`.
4. iPXE binaries are committed under `internal/boot/ipxe/bin/` with `SHA256SUMS`; `task ipxe:build` reproduces them via docker; `task ci` verifies `SHA256SUMS` and the embedded TRUST fingerprints; a test-CA variant is built for e2e only and never embedded.
5. The issuer interface in `tlsacme` exists because a real test double exists (`tlsacmetest.SelfSigned`).
6. Boot HTTPS listener TLS minimum 1.3; the iPXE v2.0.0 / TLS 1.2 fallback is a later ruling.

**Rulings made while planning (spec silent or ambiguous):**
- Ruling: the `/boot` decision lives in `internal/boot` — §5.1 assigns "the decision (§5.2)" to `boot`; the controller's scope list grouped it with fleet — cost if wrong: a package move.
- Ruling: `settings.unknown_action` is seeded `local` — the §5.2 cut-over runbook starts with `local` and M1 has no UI to set it — cost if wrong: unknown machines boot local disk instead of holding.
- Ruling: every hold (rows 2, 5, 7–10) re-chains `/boot` every 30 s; "never advances" (row 5) means the decision never moves the machine on its own — cost: one `/boot` request per held machine per 30 s, events coalesced.
- Ruling: an `assigned` machine whose UUID is junk holds at row 10 with reason `junk SMBIOS UUID: installs are refused` — §2 non-goal plus row 2's "a clone never gets an install" — cost if wrong: none (more conservative).
- Ruling: row 2 marks every record with that UUID junk, records an alert audit event, and does not attach the requesting MAC; the clone gets its own record on its next `/boot` — cost: a legitimately re-flashed board holds until the admin forgets it.
- Ruling: a MAC belongs to the most recent machine that reported it (upsert reassigns) — cost if wrong: a moved NIC shows on its new machine only.
- Ruling: machine records are capped at 1024 for creation from `/boot` and `/discover`; `/discover` is limited to 8 new-or-changed reports per source IP per 10 minutes with 4096 tracked sources; unchanged reports cost nothing (booty's values, `pkg/http/discovery.go`) — cost: tuning constants.
- Ruling: inventory reports keep the newest 10 per machine; `facts` is TEXT in both databases so byte equality detects unchanged reports (Postgres JSONB would reorder keys) — cost: none.
- Ruling: hold events coalesce: skip the insert when the machine's most recent boot event is a hold with the same reason less than 1 h before — the design's "one per machine per hour".
- Ruling: `/start` records no DB event in M1 (its wide event exists); the M4 alert adds what it needs — cost: no historical data for that alert before M4.
- Ruling: the source-subnet guard answers 403 — cost: none.
- Ruling: ACME account and certificate live in DB tables, sealed with AES-256-GCM (`crypto/cipher.NewGCMWithRandomNonce`, stdlib) with an AAD naming the row; certificate key RSA-2048 (widest iPXE compatibility), account key P-256 — cost if wrong: re-issue.
- Ruling: stored TLS state that does not decrypt under the configured data key stops `serve` (no silent re-issue) — a mixed-up data key is a misconfiguration.
- Ruling: bootstrap config comes from env and `*_FILE` only (no config file); the 1Password token is not read in M1 (its consumer is M3); `HEPHAESTUS_ACME_DIRECTORY_URL` exists so the lab can use Let's Encrypt staging — cost: none.
- Ruling: `tricorder-module` is not run (it lives in the unresolvable repo) — cost: module-wide rules unchecked until the plugin resolves.
- Ruling: the iPXE builder is pinned to `linux/arm64` (colima's native arch) and a Debian snapshot; `undionly.kpxe` is LZMA-compressed, so its TRUST set is verified at build time from the uncompressed `.tmp` ELF and recorded in `BUILDINFO`, while the `.efi` files are byte-checked in `task ci` — cost: amd64 CI runners need binfmt emulation to rebuild.
- Ruling: the `a040f9579` ancestor check runs at image build and in `build.sh`; `task ci` checks the recorded line — cost: none.
- Ruling: Postgres tests start `postgres:18-alpine` through the docker CLI (no testcontainers dependency tree) — cost: a crashed test run can leave a container (`--rm` on stop).
- Ruling: hephaestus-owned tests use stdlib `testing`; testify stays only in the vendored provider tests.
- Ruling: shutdown has no drain delay (no load balancer in front); proxyDHCP stops first.
- Ruling: no trace handler in M1 (no tracer provider and no inbound trace context from boot clients).
- Ruling: the vendored proxy's `siderolabs/gen` `xslices.Map` call becomes a loop (a little copy over a dependency).
- Ruling: library packages never log at Info+: wide events log at a level the caller passes; `tlsacme` reports errors through a caller-supplied handler.
- Ruling: `/discover` is HTTPS only; `/boot` is on both listeners and plain HTTP gets row 1.
- Ruling: the image is built and loaded for `linux/arm64` locally; the OCI `source` and `licenses` labels wait for the remote and the licence decision (§10).
- Ruling: `task e2e` is a placeholder that exits 1.

**Rulings from plan-gate pass 1 (verbatim):**
- Ruling: allow `toolchain go1.27.4` in go.mod; pin golang:1.27.4 (trixie) by digest in the Dockerfile and build image (resolve the digest); host GOTOOLCHAIN=auto fetches it. (Resolved: `golang:1.27.4-trixie@sha256:b54b2639de8c1bfe538ec5ffc45f29b1932235ac5f6d63c059853646677112d9`. Reason: GO-2026-6859, net/http, fixed in go1.27.4.)
- Ruling: tasks whose only modification of existing files is go.mod/go.sum dependency additions are `Kind: rewrite` (Tasks 3, 4, 6, 12 and any others), so no blind A/B runs on them.
- Ruling: on shutdown, serve does not wait for an in-flight ACME Obtain (lego v4.35.2 Obtain takes no context); it is abandoned at ShutdownGrace and retried next start.
- Ruling: EFI iPXE reports memsize 0 (MEMMAP_NULL); known limitation; store/display "unknown" for 0; MEMMAP_SETTINGS stays for BIOS.
- Ruling: bounded storage — keep the newest 500 boot_events per machine and at most 16 MACs per machine (oldest dropped), with repository tests.
- Ruling: /discover creating a machine writes its "discovered" audit event in the same transaction (design §5.2); test it.
- Ruling: an ACME account row that fails to decrypt also stops serve, same as the certificate row; test it.
- Ruling: add FuzzXxx with seed corpora (run as seeds by plain go test) for NormalizeMAC, NormalizeUUID, db.ParseURL, the /boot query parser and the /discover form parser, in their owning tasks; /health reports uptime (WL:421); add --log-format flag/HEPHAESTUS_LOG_FORMAT (WL:468).
- Ruling: x/crypto is bumped to v0.59.0, the first tagged version after GO-2026-5932's fix (`0.58.1-0.20261009181718-2f1f834330b2`); hephaestus never imports `golang.org/x/crypto/openpgp`.
- Ruling: `/discover` handles an identity conflict like row 2: it marks the UUID junk and records the alert once, then answers 409.
- Ruling: `Observe` returns an `Observation` struct (machine plus `Created` and `Alerted` flags) so the handler can charge and alert correctly; `Repository.Transition` takes a `TransitionRequest`.
- Ruling: `serve` builds its issuer through a factory in `serveDeps` (`newIssuer`), because the ACME account check needs the opened database; tests pass a factory that returns `tlsacmetest.SelfSigned`.
- Ruling: transition rejection reasons are snake_case tokens (`junk_uuid`, `host_not_found`, `host_bound`, `reset_not_confirmed`, `not_bound`, `already_cordoned`, `not_cordoned`, `not_allowed`); `host_not_found` takes precedence over `host_bound`.

## Parked gate findings

None parked. Gate minor 19 is covered by the Global Constraints rule that a lint rejection of a plan shape changes the shape and records a ruling; minor 20 was informational.

## Global Constraints

Every task's requirements include these.

- Module `go.jacaudi.dev/hephaestus`; `go.mod` has `go 1.26.0` (the highest `go` directive among the pinned dependencies, verified) and `toolchain go1.27.4` (GO-2026-6859). Never raise the `go` line.
- An advisory published mid-run against a pinned toolchain or module is fixed by bumping to the minimal fixed version (never a major bump) and recording a Ruling line; it is not a stop.
- Builds: `CGO_ENABLED=0`, `-trimpath`, version ldflags into `main.version`, `main.commit`, `main.date`. Tests: `go test ./... -race`.
- Canonical standards: `~/Projects/github/go-tricorder/docs/standards/go-standards.md` (binding over this plan's shapes; if lint rejects a name or shape this plan gives, keep the behaviour, change the shape, record a ruling) and WL `~/Projects/local/spike/openspec/docs/workflow-lenses.md` lines 260–570.
- No `init()`; no package globals except `//go:embed` vars, `cmd/` (ldflags vars, regexps) and a nolint-justified one where a task says so; viper and cobra only in `cmd/`; no `log` package import; every exported symbol documented; package docs in `doc.go` for every package except `cmd/hephaestus` (its doc comment sits in `main.go`) and `internal/third_party/**` (upstream files as vendored).
- Logs: `log/slog`, snake_case keys, error key `error`; library code (`internal/**`) never logs at Info or above; `internal/third_party/**` keeps upstream log levels. Never log a `secret.Secret`'s value, a database URL, or a query string.
- Errors: wrap with `%w`; sentinels `ErrX`; error types end in `Error`.
- Times: persisted in UTC; repository methods take `at time.Time`; tests use `t0 := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)` and compare with `Equal`.
- Postgres tests live in `*_integration_test.go` files with `//go:build integration`; each such package has `main_integration_test.go` whose `TestMain` returns `dbtest.RunWithPostgres(m)` through `os.Exit`. Database suites are one function run as `TestX_SQLite` (untagged) and `TestX_Postgres` (tagged).
- Packages that start goroutines end their goroutine tests with `defer goleak.VerifyNone(t)`.
- Command lines: every `Run:` line that starts with `go`, `git`, `grep`, `ls` or `task` is prefixed with `rtk proxy` (a hook rewrites their output otherwise); pipelines run as `rtk proxy sh -c "…"`. Any command that needs docker (Postgres tests, `task ci` from Task 5 on, iPXE and image builds) runs with `DOCKER_HOST=unix://$HOME/.colima/default/docker.sock` exported. macOS host: no `timeout` command.
- Colima shares only `$HOME`: never bind-mount `/tmp` into a container; container scratch lives inside the container or under the repo's gitignored `build/ipxe/.work/`.
- If `DOCKER_HOST=… docker info --format '{{.ServerVersion}}'` fails (28.4.0 at plan time), stop with `PLAN: BLOCKED — environment: docker unreachable at DOCKER_HOST`. Never fabricate binaries or skip Postgres tests.
- Commits: one or more per task on `feat/hephaestus-m1-core`, signed, message ends with `-m "Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"`. A signing failure pauses the run. Builders never reset, rebase, force or delete commits; a failed task's commits are left for the controller.
- Dependencies, exact versions (verified against the module source on 2026-11-14; add each with `rtk proxy go get <module>@<version>` in the task that first imports it, and run `rtk proxy go mod tidy` after implementing, before the gate; when `rtk proxy go list -m golang.org/x/crypto` shows a version below v0.59.0 after a task's `go get` or tidy, run `rtk proxy go get golang.org/x/crypto@v0.59.0` in that task):

| Module | Version | First task |
|---|---|---|
| github.com/spf13/cobra | v1.10.2 | 1 |
| go.opentelemetry.io/otel, /otel/metric, /otel/sdk/metric | v1.47.0 | 3 |
| go.opentelemetry.io/otel/exporters/prometheus | v0.69.0 | 3 |
| github.com/prometheus/client_golang | v1.25.0 | 3 |
| go.opentelemetry.io/contrib/instrumentation/runtime, …/net/http/otelhttp | v0.72.0 | 3 |
| go.uber.org/goleak | v1.3.0 | 4 |
| golang.org/x/sys | v0.50.0 | 4 |
| github.com/uptrace/bun, bun/dialect/sqlitedialect, bun/dialect/pgdialect | v1.3.0 | 5 |
| modernc.org/sqlite | v1.62.0 | 5 |
| github.com/jackc/pgx/v5 | v5.13.0 | 5 |
| github.com/pressly/goose/v3 | v3.30.1 | 5 |
| github.com/spf13/viper | v1.21.0 | 6 |
| github.com/go-viper/mapstructure/v2 | v2.5.0 | 6 |
| github.com/insomniacslk/dhcp | v0.0.0-20261014062924-c6bb46ad3f3a | 12 |
| github.com/pin/tftp/v3 | v3.2.0 | 12 |
| golang.org/x/sync | v0.25.0 | 12 |
| github.com/stretchr/testify (vendored tests only) | v1.12.1 | 12 |
| github.com/go-acme/lego/v4 | v4.35.2 | 15 |
| golang.org/x/crypto (indirect; GO-2026-5932) | v0.59.0 | first task that pulls it in |

- If `go mod tidy` changes `go.mod`/`go.sum` in a task that does not list them, include them in that task's commit (dependency bookkeeping only; never a version other than the table's).
- Tools pinned by `go run`: golangci-lint `github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.14.0`, govulncheck `golang.org/x/vuln/cmd/govulncheck@v1.9.0`. hadolint is the host binary.
- Images: `debian:trixie-slim@sha256:a72b4433de1cf7d6bae0c4cee74d0547f58601a643b3375f070c51600d63b6b2`, `golang:1.27.4-trixie@sha256:b54b2639de8c1bfe538ec5ffc45f29b1932235ac5f6d63c059853646677112d9`, `gcr.io/distroless/static-debian13:nonroot@sha256:aafe993665bf6200db38a02c11a9351e782a84b6a45e7f33b6695638c88e3931`, `postgres:18-alpine@sha256:1018733457011bf88d81db98cf43aac6d5150afc18787dbe204771fedf80ff95`. Debian snapshot `20261104T212339Z`.
- Every commit step's Expected includes: `rtk proxy sh -c "git cat-file commit HEAD | grep -c '^gpgsig'"` prints `1`.
- RED steps: a test that cannot compile is the expected failure; builders never add stubs to reach a different failure.
- Fuzz tests (`FuzzXxx`) seed `f.Add` with every input of the matching table test, so plain `go test` replays them; the owning task also runs each for 30 s once (`-run '^$' -fuzz=FuzzXxx -fuzztime=30s`), and a crasher it finds is committed under `testdata/fuzz/` with the fix.
- Listeners in production code and tests use `net.ListenConfig{}` `.Listen(ctx, …)` (noctx).
- Fixtures shared by tests: FQDN `hephaestus.example.test`; provisioning subnet `10.10.20.0/24`, address `10.10.20.5`, client `10.10.20.50`; UUID `4c4c4544-0042-3510-8052-b4c04f4e4d32`; MACs `aa:bb:cc:00:00:01` upward; data key `AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8=` (bytes 0x00–0x1f).

## Review Focus

The input classes and failure modes most likely to bite, each pinned to a test.

1. **Hostile or odd boot input** (newlines, `${…}`, `#!ipxe`, 300-byte values, malformed MACs, missing UUID and MAC) must never reach a script and never 500, and per-machine storage stays bounded (500 boot events, 16 MACs, 10 reports). Pinned: Task 13 `TestBoot_RequestTextNeverEchoed`, `TestBoot_OversizeParamRejected`, `FuzzParseBootQuery`; Task 14 `TestDiscover_RejectsOversizeAndMalformed`, `FuzzParseDiscoverForm`; Task 8 suite cases `BootEventsCapped`, `MACsCapped`; Task 9 `ReportRetention`.
2. **Identity edge cases**: a clone (same UUID, other serial, new MAC), a multi-NIC machine, two all-zero-UUID machines, a NIC moving to a new UUID, a junk UUID seen on a new NIC of the original. Pinned: Task 8 suite cases `MultiNICSameMachine`, `ZeroUUIDKeyedByMAC`, `MarkJunkOnConflict`, `CloneAfterJunkGetsOwnRecord`, `JunkUUIDMatchedByPairOnNewNIC`, `MACMovesToNewMachine`.
3. **Source address handling**: IPv4-mapped IPv6 clients, IPv6 clients, unparsable RemoteAddr on boot routes; an admin address that is a wildcard or inside the provisioning subnet. Pinned: Task 13 `TestRequireSubnet`; Task 6 `TestLoadConfig_Invalid` rows `admin wildcard` and `admin on VLAN`.
4. **Restart with existing state**: a stored certificate is reused without re-issue; a wrong data key stops startup loudly; a certificate for a changed FQDN is ignored and re-issued. Pinned: Task 15 `TestLoad_ReusesStoredCertificate`, `TestLoad_WrongDataKeyFailsLoudly`, `TestLoad_OtherDomainIgnored`; Task 16 `TestRunServe_WrongDataKeyFails`.
5. **Hold-loop load**: a held machine re-chains every 30 s and re-reports; events coalesce and identical reports neither insert nor spend rate budget. Pinned: Task 8 `HoldCoalescing`; Task 9 `ReportStoredThenDeduped`; Task 14 `TestDiscover_UnchangedNotCharged`.

---

### Task 1: Repository skeleton, taskfile, lint, version command

**Files:**
- Create: `go.mod`, `go.sum`, `.gitignore`, `.golangci.yml`, `taskfile.yml`, `.taskfiles/go.yml`, `.taskfiles/repo.yml`
- Create: `cmd/hephaestus/main.go`, `cmd/hephaestus/version.go`, `cmd/hephaestus/main_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces (package `main`): `var version = "dev"; commit = "unknown"; date = "unknown"`; `func newRootCmd() *cobra.Command` (Use `hephaestus`, `SilenceUsage: true`, `SilenceErrors: true`, `Args` rejects any positional argument with a `usageError`, `RunE` prints help, flag errors wrapped in `usageError` via `SetFlagErrorFunc`, `Long` and `Example` set); `func newVersionCmd() *cobra.Command` (local `--json` flag); `func buildVersion() string` (go-standards §9.2 body); `type usageError struct{ err error }` with `Error`/`Unwrap`; `func exitCode(err error) int` (nil→0, usageError→2, other→1). `main` runs `newRootCmd().ExecuteContext(ctx)` with a `signal.NotifyContext(SIGINT, SIGTERM)` context and exits with `exitCode`; for any error it first writes `hephaestus: <err>` once to `os.Stderr`.

- [ ] **Step 1: Initialise the module**

Run: `rtk proxy go mod init go.jacaudi.dev/hephaestus` then `rtk proxy go mod edit -go=1.26.0 -toolchain=go1.27.4` then `rtk proxy go get github.com/spf13/cobra@v1.10.2` then `rtk proxy go version`
Expected: `go.mod` contains `module go.jacaudi.dev/hephaestus`, `go 1.26.0`, `toolchain go1.27.4`, and requires `github.com/spf13/cobra v1.10.2`; `go version` prints `go version go1.27.4 darwin/arm64` (GOTOOLCHAIN=auto downloaded it).

- [ ] **Step 2: Write the failing tests** in `cmd/hephaestus/main_test.go`

```go
func TestVersionCmd_JSON(t *testing.T)            // args ["version","--json"] → decodes to {"version":"dev","commit":"unknown","date":"unknown"}
func TestVersionCmd_Text(t *testing.T)            // args ["version"] → out == "hephaestus dev (commit unknown, built unknown)\n"
func TestExitCode(t *testing.T)                   // exitCode(nil)==0; exitCode(usageError{errors.New("x")})==2; exitCode(errors.New("x"))==1; exitCode(fmt.Errorf("w: %w", usageError{…}))==2
func TestRoot_UnknownCommandIsUsageError(t *testing.T) // args ["bogus"] → exitCode(err)==2
func TestRoot_UnknownFlagIsUsageError(t *testing.T)    // args ["--bogus"] → exitCode(err)==2
```
Each test builds `newRootCmd()`, sets `SetArgs`, `SetOut`/`SetErr` to buffers, calls `ExecuteContext(t.Context())`.

- [ ] **Step 3: Run to verify failure**

Run: `rtk proxy go test ./cmd/hephaestus/ -v`
Expected: FAIL, `undefined: newRootCmd`.

- [ ] **Step 4: Implement `main.go` and `version.go`** with the signatures above. JSON output is pretty-printed with two-space indent, keys `version`, `commit`, `date`.

- [ ] **Step 5: Run to verify pass**

Run: `rtk proxy go test ./cmd/hephaestus/ -race -v`
Expected: PASS, 5 tests.

- [ ] **Step 6: Add the build files**

`.gitignore`, exactly:
```
/bin/
/build/ipxe/.work/
/build/ipxe/out/
```
`.taskfiles/repo.yml`: `git -C ~/Projects/github/go-tricorder show 5300050deb1b743f93ce92a7b6adedec391dd285:.taskfiles/repo.yml > .taskfiles/repo.yml` (verbatim).

`.golangci.yml`: `git -C ~/Projects/github/go-tricorder show 5300050deb1b743f93ce92a7b6adedec391dd285:config/golangci.yml > .golangci.yml`, then exactly these six edits:
1. Replace the first comment line with `# hephaestus: go-tricorder baseline (config/golangci.yml @ 5300050) minus the tricorder plugin, which cannot resolve (private repo). Restore it with .custom-gcl.yml once it does.`
2. Delete the two lines `    # go-tricorder's own analyzers` and `    - tricorder` under `linters.enable`.
3. Delete the whole `settings.custom` block (from `    # go-tricorder's analyzers (design §4.3)…` through `          disable: []`).
4. Delete the exclusion rule whose `path` is `^internal/analyzers/`: its comment line (`# deviation: var Analyzer is the go/analysis idiom…`) and its four lines (`- path: ^internal/analyzers/`, `linters:`, `- gochecknoglobals`, `text: ^Analyzer is a global variable`).
5. Add `      - ^internal/third_party/` after `      - builtin$` in both `linters.exclusions.paths` and `formatters.exclusions.paths`.
6. Set `formatters.settings.goimports.local-prefixes` to `go.jacaudi.dev/hephaestus`.

`.taskfiles/go.yml`, exactly:
```yaml
version: '3'

vars:
  GOLANGCI: go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.14.0
  VERSION: {sh: git describe --tags --always --dirty}
  COMMIT: {sh: git rev-parse --short HEAD}
  DATE: {sh: date -u +%Y-%m-%dT%H:%M:%SZ}

tasks:
  ci:
    desc: All Go checks
    cmds:
      - task: fmt:check
      - task: tidy:check
      - task: lint
      - task: vuln
      - task: test
      - task: cross
  lint:
    desc: golangci-lint v2.14.0 with the go-tricorder baseline (stock binary; see .golangci.yml head)
    cmds:
      - '{{.GOLANGCI}} run'
  fmt:
    desc: Format Go sources
    cmds:
      - '{{.GOLANGCI}} fmt'
  fmt:check:
    desc: Fail if sources are unformatted
    cmds:
      - '{{.GOLANGCI}} fmt --diff'
  tidy:check:
    desc: Fail if go.mod/go.sum are not tidy
    cmds:
      - go mod tidy -diff
  vuln:
    desc: Scan dependencies for known vulnerabilities
    cmds:
      - go run golang.org/x/vuln/cmd/govulncheck@v1.9.0 ./...
  test:
    desc: Unit tests with the race detector
    cmds:
      - go test ./... -race
  build:
    desc: Build bin/hephaestus
    env:
      CGO_ENABLED: '0'
    cmds:
      - go build -trimpath -ldflags "-X main.version={{.VERSION}} -X main.commit={{.COMMIT}} -X main.date={{.DATE}}" -o bin/hephaestus ./cmd/hephaestus
  cross:
    desc: Every package cross-compiles for linux and darwin, amd64 and arm64 (go-standards §9.4)
    env:
      CGO_ENABLED: '0'
    cmds:
      - |
        set -eu
        for p in linux/amd64 linux/arm64 darwin/amd64 darwin/arm64; do
          GOOS="${p%/*}" GOARCH="${p#*/}" go build -o /dev/null ./...
        done
```
`taskfile.yml`, exactly:
```yaml
# THE CI CONTRACT (github-actions-align): `task ci` is what CI runs and what you run locally.
version: '3'

includes:
  repo:
    taskfile: .taskfiles/repo.yml
  go:
    taskfile: .taskfiles/go.yml

tasks:
  default:
    desc: List all tasks
    cmds:
      - task --list
  ci:
    desc: Everything CI runs. Run this before committing.
    cmds:
      - task: repo:ci
      - task: go:ci
  lint:
    desc: Static checks only
    cmds:
      - task: repo:ci
      - task: go:lint
  test:
    desc: Tests only
    cmds:
      - task: go:test
  build:
    desc: Build artefacts
    cmds:
      - task: go:build
  e2e:
    desc: QEMU end-to-end test (Linux only, design §7.6); not part of ci
    cmds:
      - |
        echo "e2e: the QEMU harness arrives in a later milestone" >&2
        exit 1
```

- [ ] **Step 7: Run the full gate**

Run: `rtk proxy sh -c "task --list-all >/dev/null && echo taskfile-ok"` — Expected: `taskfile-ok` (every taskfile parses).
Run: `rtk proxy sh -c "go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.14.0 config verify && echo verify-ok"` — Expected: `verify-ok`.
Run: `rtk proxy task ci`
Expected: exit 0; lint prints `0 issues.`; govulncheck prints `No vulnerabilities found.`; hadolint step prints `no Dockerfile — skipping hadolint`.
Run: `rtk proxy task build` then `./bin/hephaestus version --json`
Expected: JSON whose `commit` is the short HEAD hash.

- [ ] **Step 8: Commit**

Run: `rtk proxy git add go.mod go.sum .gitignore .golangci.yml taskfile.yml .taskfiles cmd` then `rtk proxy git commit -m "feat: repository skeleton, taskfile, lint baseline, version command" -m "Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"`
Expected: one new commit; `rtk proxy sh -c "git cat-file commit HEAD | grep -c '^gpgsig'"` prints `1`.

---

### Task 2: Secret type and data key

**Files:**
- Create: `internal/secret/doc.go`, `internal/secret/secret.go`, `internal/secret/secret_test.go`
- Create: `internal/datakey/doc.go`, `internal/datakey/datakey.go`, `internal/datakey/datakey_test.go`

**Interfaces:**
- Produces `secret.Secret` (named `string`) with exactly go-standards §15.2's methods and bodies: `String`, `Format`, `LogValue`, `MarshalJSON`, `MarshalText`, `Expose`.
- Produces `datakey.Size = 32`; `var datakey.ErrDecrypt = errors.New("datakey: decrypt failed: wrong data key or corrupted data")`; `type Key struct{ aead cipher.AEAD }`; `func Parse(encoded secret.Secret) (*Key, error)`; `func (k *Key) Seal(plaintext, aad []byte) []byte`; `func (k *Key) Open(sealed, aad []byte) ([]byte, error)`; `func (k *Key) String() string` and `LogValue() slog.Value` both returning `****`.

- [ ] **Step 1: Write the failing tests**

```go
// secret_test.go
func TestSecret_Redacts(t *testing.T) // s := Secret("hunter2"): fmt.Sprintf for %s %v %+v %#v %d %x each == "****"; json.Marshal == `"****"`; MarshalText == "****"; slog JSON line of slog.Any("k", s) contains `"k":"****"` and not "hunter2"; s.Expose() == "hunter2"
// datakey_test.go  (fixture key: Parse("AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8="))
func TestSealOpen_RoundTrip(t *testing.T)   // Open(Seal([]byte("acme key"), []byte("aad-1")), []byte("aad-1")) == "acme key"; len(sealed) == len(plaintext)+28
func TestSeal_RandomNonce(t *testing.T)     // two Seals of the same plaintext differ
func TestOpen_Rejects(t *testing.T)         // table: wrong aad; one flipped byte; a different key (32 bytes of 0xff); truncated to 10 bytes → each errors.Is(err, ErrDecrypt)
func TestParse_Rejects(t *testing.T)        // "" and "not-base64!" → error containing "not valid base64" or "decoded 0 bytes"; "AAECAwQFBgcICQoLDA0ODw==" → error containing "decoded 16 bytes, want 32 (generate one with: openssl rand -base64 32)"
func TestParse_TrimsWhitespace(t *testing.T) // fixture + "\n" parses
func TestKey_Redacts(t *testing.T)          // fmt.Sprint(key) == "****"
```

- [ ] **Step 2: Run to verify failure**

Run: `rtk proxy go test ./internal/secret/ ./internal/datakey/ -v`
Expected: FAIL, `undefined: Secret` / `undefined: Parse`.

- [ ] **Step 3: Implement.** `Parse` trims whitespace, decodes `base64.StdEncoding` (error `data key: not valid base64: %w`), checks length (`data key: decoded %d bytes, want 32 (generate one with: openssl rand -base64 32)`), builds `aes.NewCipher` + `cipher.NewGCMWithRandomNonce`. `Open` returns `ErrDecrypt` for any failure.

- [ ] **Step 4: Run to verify pass**

Run: `rtk proxy go test ./internal/secret/ ./internal/datakey/ -race -v`
Expected: PASS.

- [ ] **Step 5: Gate and commit**

Run: `rtk proxy task ci` — Expected: exit 0.
Run: `rtk proxy git add internal/secret internal/datakey` then `rtk proxy git commit -m "feat: secret type and AES-GCM data key" -m "Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"`
Expected: one new commit; `rtk proxy sh -c "git cat-file commit HEAD | grep -c '^gpgsig'"` prints `1`.

---

### Task 3: Telemetry — logger, wide events, metrics
Kind: rewrite

**Files:**
- Create: `internal/telemetry/doc.go`, `logger.go`, `wide.go`, `metrics.go`, `logger_test.go`, `wide_test.go`, `metrics_test.go`
- Modify: `go.mod`
- Modify: `go.sum`

The only change to existing files is dependency additions (`Kind: rewrite` per the go.mod ruling).

**Interfaces:**
- Produces:
  - `func NewLogger(w io.Writer, format string, level slog.Level, version string) (*slog.Logger, error)` — `json` → `slog.NewJSONHandler`, `text` → `slog.NewTextHandler`, else error `unknown log format %q`; logger carries `service=hephaestus` and `version=<version>`.
  - `func WideEvents(next http.Handler, logger *slog.Logger, level slog.Level) http.Handler` — one `logger.LogAttrs(ctx, level, "request", …)` per request after `next` returns, with attrs `method`, `path` (`r.URL.Path`), `query` = `"[redacted]"` only when `r.URL.RawQuery != ""`, `status` (200 when the handler never called `WriteHeader`), `bytes`, `duration_ms` (float64, milliseconds), `remote_ip` (host part of `RemoteAddr`), `tls` (bool), then every attr added with `AddAttrs`. Its `ResponseWriter` wrapper implements `Unwrap() http.ResponseWriter`.
  - `func AddAttrs(ctx context.Context, attrs ...slog.Attr)` — appends to the current request's event; no-op without one; safe for concurrent use.
  - `type Metrics struct` (unexported fields); `func NewMetrics(version, commit string) (*Metrics, error)` — own `prometheus.NewRegistry()`, `otelprom.New(otelprom.WithRegisterer(reg))`, `sdkmetric.NewMeterProvider(sdkmetric.WithReader(exp))`, `runtime.Start(runtime.WithMeterProvider(mp))`, and an `Int64ObservableGauge` `hephaestus.build.info` observing 1 with attrs `version`, `commit`, `go_version` (`runtime.Version()` from package `runtime` aliased as `goruntime`); `(*Metrics).Meter() metric.Meter` (scope `go.jacaudi.dev/hephaestus`; carries `//nolint:ireturn // OTel API returns the interface`, the go-standards §12 carve-out); `(*Metrics).Handler() http.Handler` (`promhttp.HandlerFor(reg, promhttp.HandlerOpts{})`); `(*Metrics).Instrument(h http.Handler, operation string) http.Handler` (`otelhttp.NewHandler(h, operation, otelhttp.WithMeterProvider(mp))`); `(*Metrics).Shutdown(ctx context.Context) error`.

- [ ] **Step 1: Add dependencies**

Run: `rtk proxy go get go.opentelemetry.io/otel@v1.47.0 go.opentelemetry.io/otel/metric@v1.47.0 go.opentelemetry.io/otel/sdk/metric@v1.47.0 go.opentelemetry.io/otel/exporters/prometheus@v0.69.0 github.com/prometheus/client_golang@v1.25.0 go.opentelemetry.io/contrib/instrumentation/runtime@v0.72.0 go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp@v0.72.0`
Expected: exit 0; `go 1.26.0` unchanged.

- [ ] **Step 2: Write the failing tests**

```go
func TestNewLogger_JSON(t *testing.T)           // InfoContext(ctx,"hello", "user_id","u1") → one JSON line: msg "hello", level "INFO", service "hephaestus", version "v1.2.3", user_id "u1"
func TestNewLogger_RejectsUnknownFormat(t *testing.T) // "xml" → error containing `unknown log format "xml"`
func TestWideEvents_OneLinePerRequest(t *testing.T)   // handler: AddAttrs(ctx, slog.String("decision","hold")); WriteHeader(202); Write("abc"). Request GET /boot?uuid=1&t=secret-ticket, RemoteAddr "10.10.20.50:41000". Exactly one line: msg "request", method "GET", path "/boot", query "[redacted]", status 202, bytes 3, remote_ip "10.10.20.50", tls false, decision "hold", duration_ms is a number >= 0; the line contains neither "secret-ticket" nor "uuid=1"
func TestWideEvents_NoQueryKeyWithoutQuery(t *testing.T) // GET /start → decoded line has no "query" key
func TestWideEvents_DefaultStatus200(t *testing.T)       // handler only writes "x" → status 200
func TestWideEvents_ResponseControllerReachesConn(t *testing.T) // httptest.NewServer(WideEvents(h,…)); h calls http.NewResponseController(w).SetWriteDeadline(time.Now().Add(time.Second)) → err == nil, then writes "ok"; client sees 200 "ok"
func TestAddAttrs_NoEventIsNoop(t *testing.T)            // AddAttrs(context.Background(), slog.Int("x",1)) does not panic
func TestNewMetrics_BuildInfoAndRuntime(t *testing.T)    // NewMetrics("v1.2.3","abc1234"); GET Handler() body contains `hephaestus_build_info{` and `version="v1.2.3"` and `commit="abc1234"` and `go_goroutine_count`
func TestInstrument_RecordsRED(t *testing.T)             // serve one request through Instrument(h,"boot") → metrics body contains "http_server_request_duration_seconds"
```

- [ ] **Step 3: Run to verify failure**

Run: `rtk proxy go test ./internal/telemetry/ -v`
Expected: FAIL, `undefined: NewLogger`.

- [ ] **Step 4: Implement** the signatures above.

- [ ] **Step 5: Run to verify pass**

Run: `rtk proxy go test ./internal/telemetry/ -race -v`
Expected: PASS, 9 tests.

- [ ] **Step 6: Gate and commit**

Run: `rtk proxy task ci` — Expected: exit 0.
Run: `rtk proxy git add go.mod go.sum internal/telemetry` then `rtk proxy git commit -m "feat: slog logger, wide events, OTel Prometheus metrics" -m "Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"`
Expected: one new commit; `rtk proxy sh -c "git cat-file commit HEAD | grep -c '^gpgsig'"` prints `1`.

---

### Task 4: Admin listener and check registry
Kind: rewrite

**Files:**
- Create: `internal/admin/doc.go`, `lifecycle.go`, `registry.go`, `handler.go`, `disk.go`, `lifecycle_test.go`, `registry_test.go`, `handler_test.go`, `disk_test.go`
- Modify: `go.mod`
- Modify: `go.sum`

The only change to existing files is dependency additions (`Kind: rewrite` per the go.mod ruling).

**Interfaces:**
- Produces:
  - `type State int` with `Starting`, `Ready`, `Draining`, `Stopped` (iota from 0) plus `String`, `MarshalText`, `UnmarshalText` (go-standards §13.2 bodies); `type Lifecycle struct{ state atomic.Int64 }` with `Set(State)` and `Current() State`.
  - `type Status string` with `StatusPass = "pass"`, `StatusWarn = "warn"`, `StatusFail = "fail"`.
  - `type Result struct { Status Status; ObservedValue any; ObservedUnit string; Output string }`.
  - `type Check struct { Name string; Critical bool; Interval time.Duration; Run func(context.Context) Result }` (Interval 0 means 15 s; each run gets a 5 s `context.WithTimeout`).
  - `func NewRegistry(checks ...Check) *Registry`; `(*Registry).RunOnce(ctx context.Context)` (every check once, synchronously); `(*Registry).Run(ctx context.Context)` (each check on its own ticker until ctx is done; returns after all its goroutines exit).
  - `type Info struct { Version, ReleaseID string; Started time.Time }`; `func NewHandler(reg *Registry, life *Lifecycle, metrics http.Handler, info Info) http.Handler` serving only `GET /healthz`, `GET /readyz`, `GET /health`, `GET /metrics`.
  - `func DiskCheck(name, path string, minFreeRatio float64) Check` — informational; `unix.Statfs`; observed value = free percentage computed as `float64(st.Bavail) / float64(st.Blocks) * 100` (no integer conversion, no nolint directive), unit `%`; `warn` with output `free space <p>% is below <min>%` when free/total < minFreeRatio; `fail` with the statfs error as output.
- Response contract:
  - `/healthz`: 200 `{"status":"ok"}` unless lifecycle is `Stopped` (503 `{"status":"stopped"}`).
  - `/readyz`: 200 body `ok\n` when lifecycle is `Ready` and every critical check's latest result is `pass`; otherwise 503 with one line per problem: `fail: lifecycle: <state>\n`, `fail: <name>: <output>\n`, `fail: <name>: pending\n` (never ran). `?verbose` appends `<status>: <name>\n` for every check, sorted by name.
  - `/health`: `Content-Type: application/health+json`; body `{"status":…, "version": info.Version, "releaseId": info.ReleaseID, "serviceId":"hephaestus", "checks": {"<name>":[{"status","observedValue","observedUnit","time","output"}]}}` (`observedValue`, `observedUnit`, `output` omitted when empty; `time` RFC 3339 UTC of the run). `checks` always also holds `"uptime"` (`pass`, observed seconds since `info.Started` as float64, unit `s`, WL:421) and `"lifecycle"` (`pass` when Ready, else `fail` with the state name as output, e.g. `draining`). The lifecycle entry counts as critical. One rule decides both probes: a critical `fail` or a pending critical makes `/readyz` 503 and `/health` `fail` 503; otherwise any `warn` or informational `fail` makes `/health` `warn` 200; else `pass` 200.

- [ ] **Step 1: Add dependencies**

Run: `rtk proxy go get go.uber.org/goleak@v1.3.0 golang.org/x/sys@v0.50.0`
Expected: exit 0.

- [ ] **Step 2: Write the failing tests**

```go
func TestState_TextRoundTrip(t *testing.T)            // each state MarshalText→UnmarshalText; UnmarshalText("bogus") errors; MarshalText(State(9)) errors
func TestHealthz(t *testing.T)                        // Ready → 200 `{"status":"ok"}`; Draining → 200; Stopped → 503
func TestReadyz_PendingCriticalCheck(t *testing.T)    // Ready, critical "database:ping" never run → 503, body contains "fail: database:ping: pending"
func TestReadyz_AllPass(t *testing.T)                 // after RunOnce with a passing critical check → 200 "ok\n"
func TestReadyz_InformationalNeverFails(t *testing.T) // informational check returning fail → 200
func TestReadyz_Draining(t *testing.T)                // lifecycle Draining → 503 body contains "fail: lifecycle: draining"
func TestReadyz_Verbose(t *testing.T)                 // ?verbose lists "pass: a" and "warn: b" lines
func TestHealth_Format(t *testing.T)                  // critical "database:ping" pass observed 1.5 "ms"; informational "tls:expiry" warn observed 10 "d" output "certificate expires in 10 days"; Info{"v1.2.3","abc1234"} → 200, content-type "application/health+json", status "warn", version "v1.2.3", releaseId "abc1234", serviceId "hephaestus", checks["database:ping"][0] status "pass" observedValue 1.5 observedUnit "ms", time parses as RFC 3339, checks["tls:expiry"][0].output as given
func TestHealth_CriticalFail503(t *testing.T)         // critical fail → 503, status "fail"
func TestHealth_InformationalFailIsWarn(t *testing.T) // informational fail only → 200, status "warn"
func TestHealth_Uptime(t *testing.T)                  // Info.Started = time.Now().Add(-90*time.Second) → checks["uptime"][0]: status "pass", observedUnit "s", observedValue >= 90
func TestHealth_Draining(t *testing.T)                // lifecycle Draining → 503, status "fail", checks["lifecycle"][0].output "draining"
func TestMetricsRoute(t *testing.T)                   // GET /metrics → body "metrics-ok" from the supplied handler
func TestOnlyFourRoutes(t *testing.T)                 // GET /ping, /ready, /livez → 404
func TestRegistry_RunStopsOnCancel(t *testing.T)      // goleak; Run with Interval 10ms; cancel after the check ran twice; Run returns
func TestDiskCheck(t *testing.T)                      // DiskCheck("disk:data_dir", t.TempDir(), 0) → pass, unit "%"; minFreeRatio 1.01 → warn; path "/nonexistent-hephaestus" → fail
```

- [ ] **Step 3: Run to verify failure**

Run: `rtk proxy go test ./internal/admin/ -v`
Expected: FAIL, `undefined: NewRegistry`.

- [ ] **Step 4: Implement** the signatures and response contract above.

- [ ] **Step 5: Run to verify pass**

Run: `rtk proxy go test ./internal/admin/ -race -v`
Expected: PASS, 16 tests.

- [ ] **Step 6: Gate and commit**

Run: `rtk proxy task ci` — Expected: exit 0.
Run: `rtk proxy git add go.mod go.sum internal/admin` then `rtk proxy git commit -m "feat: admin listener with a health check registry" -m "Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"`
Expected: one new commit; `rtk proxy sh -c "git cat-file commit HEAD | grep -c '^gpgsig'"` prints `1`.

---

### Task 5: Database, migrations, test databases, coverage gate
Kind: rewrite

**Files:**
- Create: `internal/db/doc.go`, `db.go`, `checks.go`, `migrations/sqlite/00001_init.sql`, `migrations/postgres/00001_init.sql`, `db_test.go`, `migrations_test.go`, `db_integration_test.go`, `main_integration_test.go`
- Create: `internal/db/dbtest/doc.go`, `internal/db/dbtest/dbtest.go`
- Modify: `.taskfiles/go.yml`
- Modify: `go.mod`
- Modify: `go.sum`

`.taskfiles/go.yml` gains the `test-integration` and `cover:db` tasks, both added to `ci`.

**Interfaces:**
- Consumes: `secret.Secret`, `admin.Check`, `admin.Result`.
- Produces (package `db`):
  - `type Dialect string` with `DialectSQLite = "sqlite"`, `DialectPostgres = "postgres"`.
  - `func ParseURL(raw string) (Dialect, string, error)` — `sqlite:<path>` and `sqlite://<path>` (leading `//` stripped) → `DialectSQLite`, the path, which must be absolute (error `sqlite path must be absolute`); `postgres://…` or `postgresql://…` → `DialectPostgres`, raw unchanged; anything else → error `unsupported scheme; use sqlite:/absolute/path.db or postgres://`. Errors never include `raw`.
  - `type DB struct` (unexported fields: `*bun.DB`, dialect); `func Open(ctx context.Context, url secret.Secret) (*DB, error)` — SQLite: `sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")`, `SetMaxOpenConns(1)`, `sqlitedialect.New()`; Postgres: `sql.Open("pgx", url)` (blank import `github.com/jackc/pgx/v5/stdlib`), `pgdialect.New()`; then `PingContext`. `(*DB).Bun() *bun.DB`; `(*DB).Dialect() Dialect`; `(*DB).Ping(ctx) error`; `(*DB).Migrate(ctx) error`; `(*DB).Pending(ctx) (bool, error)`; `(*DB).Close() error`.
  - Migrations: `//go:embed migrations` with `fs.Sub` per dialect; `goose.NewProvider(goose.DialectSQLite3 | goose.DialectPostgres, d.Bun().DB, sub, opts…)`; Postgres adds `goose.WithLocker(l)` with `l, err := lock.NewPostgresTableLocker()`. Never call `Provider.Close` (it closes the DB).
  - `func (d *DB) Checks() []admin.Check` — `database:ping` (critical; observed ping time float64 ms, unit `ms`) and `database:migrations` (critical; `fail` output `pending migrations` when `Pending` is true).
- Produces (package `dbtest`, a test-helper package):
  - `const PostgresImage = "postgres:18-alpine@sha256:1018733457011bf88d81db98cf43aac6d5150afc18787dbe204771fedf80ff95"`.
  - `func SQLite(t testing.TB) *db.DB` — opens `sqlite:<t.TempDir()>/test.db`, migrates, `t.Cleanup(Close)`.
  - `func RunWithPostgres(m *testing.M) int` — `docker run -d --rm --label hephaestus-dbtest=1 -e POSTGRES_USER=hephaestus -e POSTGRES_PASSWORD=hephaestus -e POSTGRES_DB=hephaestus -p 127.0.0.1::5432 <PostgresImage>` via `exec.CommandContext` (inherits `DOCKER_HOST`; each call carries `//nolint:gosec // G204: fixed docker CLI args in tests`), reads the port from `docker port <id> 5432/tcp`, waits until a pgx ping succeeds (every 250 ms, 60 s limit; a TCP connect alone is not readiness: colima's forwarder accepts early), runs `m.Run()`, then `docker rm -f <id>`. On any docker failure it prints `dbtest: start postgres: <err> (is DOCKER_HOST set?)` to stderr and returns 1.
  - `func Postgres(t testing.TB) *db.DB` — `t.Fatal("dbtest: Postgres needs TestMain to call dbtest.RunWithPostgres")` when the container is not running; otherwise `CREATE DATABASE t_<16 lowercase hex>`, opens it, migrates, and on cleanup closes and `DROP DATABASE`.
  - The container address is one package-level variable marked `//nolint:gochecknoglobals // process-wide test container shared by RunWithPostgres and Postgres`.

**Schema (version 00001, both dialects, identical table and column names).** SQLite file, exactly:
```sql
-- +goose Up
CREATE TABLE machines (
    id            TEXT PRIMARY KEY,
    smbios_uuid   TEXT NOT NULL,
    junk          INTEGER NOT NULL DEFAULT 0 CHECK (junk IN (0, 1)),
    serial        TEXT NOT NULL,
    product       TEXT NOT NULL,
    manufacturer  TEXT NOT NULL DEFAULT '',
    arch          TEXT NOT NULL CHECK (arch IN ('amd64', 'arm64')),
    state         TEXT NOT NULL CHECK (state IN ('discovered', 'local-only', 'assigned', 'installing', 'install-unconfirmed', 'installed', 'rejected', 'orphaned', 'cordoned')),
    prior_state   TEXT CHECK (prior_state IS NULL OR prior_state IN ('discovered', 'local-only', 'assigned', 'installing', 'install-unconfirmed', 'installed', 'rejected', 'orphaned')),
    orphaned_from TEXT CHECK (orphaned_from IS NULL OR orphaned_from IN ('assigned', 'installing', 'install-unconfirmed', 'installed')),
    cluster_id    TEXT,
    host_name     TEXT,
    created_at    TEXT NOT NULL,
    last_seen_at  TEXT NOT NULL
) STRICT;
CREATE UNIQUE INDEX machines_valid_uuid ON machines (smbios_uuid) WHERE junk = 0;
CREATE INDEX machines_uuid ON machines (smbios_uuid);
CREATE TABLE machine_macs (
    mac        TEXT PRIMARY KEY,
    machine_id TEXT NOT NULL REFERENCES machines (id) ON DELETE CASCADE,
    seen_at    TEXT NOT NULL
) STRICT;
CREATE INDEX machine_macs_machine ON machine_macs (machine_id);
CREATE TABLE inventory_reports (
    id          INTEGER PRIMARY KEY,
    machine_id  TEXT NOT NULL REFERENCES machines (id) ON DELETE CASCADE,
    source      TEXT NOT NULL CHECK (source IN ('ipxe')),
    facts       TEXT NOT NULL,
    received_at TEXT NOT NULL
) STRICT;
CREATE INDEX inventory_reports_machine ON inventory_reports (machine_id, id);
CREATE TABLE boot_events (
    id           INTEGER PRIMARY KEY,
    machine_id   TEXT NOT NULL REFERENCES machines (id) ON DELETE CASCADE,
    at           TEXT NOT NULL,
    decision_row INTEGER NOT NULL CHECK (decision_row BETWEEN 1 AND 10),
    action       TEXT NOT NULL,
    reason       TEXT NOT NULL DEFAULT '',
    from_state   TEXT,
    to_state     TEXT
) STRICT;
CREATE INDEX boot_events_machine ON boot_events (machine_id, id);
CREATE TABLE audit_events (
    id         INTEGER PRIMARY KEY,
    at         TEXT NOT NULL,
    actor      TEXT NOT NULL,
    action     TEXT NOT NULL,
    machine_id TEXT,
    detail     TEXT NOT NULL DEFAULT '{}'
) STRICT;
CREATE INDEX audit_events_at ON audit_events (at);
CREATE TABLE settings (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
) STRICT;
INSERT INTO settings (key, value) VALUES ('unknown_action', 'local');
CREATE TABLE acme_accounts (
    directory_url TEXT PRIMARY KEY,
    key_sealed    BLOB NOT NULL,
    registration  TEXT NOT NULL,
    created_at    TEXT NOT NULL
) STRICT;
CREATE TABLE tls_certificates (
    domain       TEXT PRIMARY KEY,
    chain_sealed BLOB NOT NULL,
    key_sealed   BLOB NOT NULL,
    not_before   TEXT NOT NULL,
    not_after    TEXT NOT NULL,
    updated_at   TEXT NOT NULL
) STRICT;

-- +goose Down
DROP TABLE tls_certificates;
DROP TABLE acme_accounts;
DROP TABLE settings;
DROP TABLE audit_events;
DROP TABLE boot_events;
DROP TABLE inventory_reports;
DROP TABLE machine_macs;
DROP TABLE machines;
```
Postgres file: the same statements with exactly these substitutions: drop `STRICT`; `junk BOOLEAN NOT NULL DEFAULT FALSE` (no CHECK) and the partial index `WHERE NOT junk`; every timestamp column (`created_at`, `last_seen_at`, `seen_at`, `received_at`, `at`, `not_before`, `not_after`, `updated_at`) `TIMESTAMPTZ`; `id INTEGER PRIMARY KEY` → `id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY`; `BLOB` → `BYTEA`; `facts` stays `TEXT`.

- [ ] **Step 1: Add dependencies**

Run: `rtk proxy go get github.com/uptrace/bun@v1.3.0 github.com/uptrace/bun/dialect/sqlitedialect@v1.3.0 github.com/uptrace/bun/dialect/pgdialect@v1.3.0 modernc.org/sqlite@v1.62.0 github.com/jackc/pgx/v5@v5.13.0 github.com/pressly/goose/v3@v3.30.1`
Expected: exit 0; `go 1.26.0` unchanged.

- [ ] **Step 2: Write the failing tests** (`package db_test`)

```go
func TestParseURL(t *testing.T) // table:
//  "sqlite:/var/lib/h/h.db" → sqlite, "/var/lib/h/h.db"; "sqlite:///var/lib/h/h.db" → sqlite, "/var/lib/h/h.db"
//  "postgres://u:p@db.example.test/heph" → postgres, same string; "postgresql://u:p@db.example.test/heph" → postgres
//  "sqlite:rel.db" → error "sqlite path must be absolute"; "mysql://x" and "" → error "unsupported scheme; use sqlite:/absolute/path.db or postgres://"
//  no error message contains "u:p"
func TestMigrations_SameVersions(t *testing.T) // versions (digits before the first "_") of migrations/sqlite and migrations/postgres are equal and non-empty; read via os.ReadDir("migrations/…")
func runMigrateSuite(t *testing.T, open func(testing.TB) *db.DB) // subtests:
//  "Tables": all eight tables exist (sqlite_master / information_schema.tables)
//  "SeededUnknownAction": SELECT value FROM settings WHERE key='unknown_action' == "local"
//  "Idempotent": Migrate again → nil; Pending → false
//  "UniqueValidUUID": two machines rows with the fixture UUID and junk false → second insert errors; with junk true → allowed
//  "ChecksPass": every Check from d.Checks() run once returns StatusPass
func TestMigrate_SQLite(t *testing.T)   { runMigrateSuite(t, dbtest.SQLite) }
func TestSQLite_Strict(t *testing.T)    // sqlite_master.sql for machines contains "STRICT"; inserting junk = 'yes' errors
func TestPending_BeforeMigrate(t *testing.T) // db.Open on a fresh sqlite file → Pending true; after Migrate → false
func FuzzParseURL(f *testing.F)              // seeds: every TestParseURL input; property: never panics; on success with DialectSQLite the path is absolute; an error message never contains the input when the input contains "@"
// db_integration_test.go (//go:build integration)
func TestMigrate_Postgres(t *testing.T) { runMigrateSuite(t, dbtest.Postgres) }
// main_integration_test.go (//go:build integration)
func TestMain(m *testing.M) { os.Exit(dbtest.RunWithPostgres(m)) }
```

- [ ] **Step 3: Run to verify failure**

Run: `rtk proxy go test ./internal/db/... -v`
Expected: FAIL with `[build failed]` or `[setup failed]` (`no non-test Go files` / `no required module provides package go.jacaudi.dev/hephaestus/internal/db…`).

- [ ] **Step 4: Implement** `db`, the two migration files and `dbtest`.

- [ ] **Step 5: Run to verify pass on both databases**

Run: `rtk proxy go test ./internal/db/... -race -v`
Expected: PASS (SQLite).
Run: `DOCKER_HOST=unix://$HOME/.colima/default/docker.sock rtk proxy go test -tags integration ./internal/db/... -race -run Postgres -v`
Expected: PASS, `TestMigrate_Postgres` with 5 subtests; then `rtk proxy sh -c "DOCKER_HOST=unix://$HOME/.colima/default/docker.sock docker ps -q --filter label=hephaestus-dbtest | wc -l | tr -d ' '"` prints `0` (no leaked container).
Run: `rtk proxy go test ./internal/db/ -run '^$' -fuzz=FuzzParseURL -fuzztime=30s` — Expected: PASS.

- [ ] **Step 6: Extend `.taskfiles/go.yml`** — add `- task: test-integration` and `- task: cover:db` after `- task: cross` in `ci`, plus these tasks and the variable `DB_PKGS: ./internal/db/...`:
```yaml
  test-integration:
    desc: Integration tests (Postgres via docker; needs DOCKER_HOST)
    cmds:
      - go test -tags integration ./... -race
  cover:db:
    desc: Every exported method in db.go and repository.go files runs on SQLite and on Postgres (WL database rule 2)
    cmds:
      - mkdir -p bin
      - go test -coverprofile=bin/cover-sqlite.out {{.DB_PKGS}}
      - go test -tags integration -run Postgres -coverprofile=bin/cover-postgres.out {{.DB_PKGS}}
      - |
        set -eu
        for f in bin/cover-sqlite.out bin/cover-postgres.out; do
          go tool cover -func="$f" | awk -v f="$f" '$1 ~ /\/(db|repository)\.go:/ && $2 ~ /^[A-Z]/ && $3 == "0.0%" {print f ": not executed: " $1 " " $2; bad=1} END {exit bad}'
        done
```

- [ ] **Step 7: Gate and commit**

Run: `rtk proxy sh -c "task --list-all >/dev/null && echo taskfile-ok"` — Expected: `taskfile-ok`.
Run: `DOCKER_HOST=unix://$HOME/.colima/default/docker.sock rtk proxy task ci`
Expected: exit 0; `cover:db` prints nothing.
Run: `rtk proxy git add go.mod go.sum .taskfiles/go.yml internal/db` then `rtk proxy git commit -m "feat: bun database with goose migrations for SQLite and Postgres" -m "Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"`
Expected: one new commit; `rtk proxy sh -c "git cat-file commit HEAD | grep -c '^gpgsig'"` prints `1`.

---

### Task 6: Bootstrap configuration
Kind: rewrite

**Files:**
- Create: `cmd/hephaestus/config.go`, `cmd/hephaestus/config_test.go`
- Modify: `cmd/hephaestus/main.go`
- Modify: `go.mod`
- Modify: `go.sum`

`main.go` gains the root persistent flag `--log-format` (WL:468); `go.mod`/`go.sum` gain dependencies.

**Interfaces:**
- Consumes: `secret.Secret`, `datakey.Parse`, `db.ParseURL`.
- Produces (package `main`):
  - `type rawConfig struct` with mapstructure-tagged fields: `FQDN string "fqdn"`; `Provisioning struct{ Interface "interface"; Address "address"; Subnet "subnet" string }` `"provisioning"`; `Admin struct{ Address string "address" }` `"admin"`; `Data struct{ Dir string "dir"; Key secret.Secret "key"; KeyFile string "key_file" }` `"data"`; `Database struct{ URL secret.Secret "url" }` `"database"`; `Cloudflare struct{ Token secret.Secret "token"; TokenFile string "token_file" }` `"cloudflare"`; `ACME struct{ DirectoryURL string "directory_url" }` `"acme"`; `ShutdownGrace time.Duration "shutdown_grace"`; `Log struct{ Format string "format"; Level slog.Level "level" }` `"log"`.
  - `type serveConfig struct { FQDN, Interface string; Address netip.Addr; Subnet netip.Prefix; AdminAddr netip.AddrPort; DataDir string; DatabaseURL secret.Secret; DataKey *datakey.Key; CloudflareToken secret.Secret; ACMEDirectoryURL string; ShutdownGrace time.Duration; LogFormat string; LogLevel slog.Level }`.
  - Root persistent flag (in `newRootCmd`): `--log-format` (string, default `""`, usage `log format: json or text (overrides HEPHAESTUS_LOG_FORMAT)`).
  - `func loadConfig(fs *pflag.FlagSet) (serveConfig, error)` — `viper.NewWithOptions(viper.ExperimentalBindStruct())`, `BindPFlag("log.format", fs.Lookup("log-format"))` (precedence flag, then env, then default), `SetEnvPrefix("HEPHAESTUS")`, `SetEnvKeyReplacer(strings.NewReplacer(".", "_"))`, `AutomaticEnv()`, defaults `admin.address=127.0.0.1:9090`, `data.dir=/var/lib/hephaestus`, `acme.directory_url=https://acme-v02.api.letsencrypt.org/directory`, `shutdown_grace=60s`, `log.format=json`, `log.level=info`; no config file; `Unmarshal` with the go-standards §10 decode hook; decode errors wrap as `decode config: %w`. Then every check below runs and all failures return together through `errors.Join`.
- Validation (exact messages):
  - `HEPHAESTUS_FQDN is required`; `HEPHAESTUS_FQDN %q: must be a fully qualified DNS name` (lowercased first; ≥2 labels, each `^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`, total ≤253).
  - `HEPHAESTUS_PROVISIONING_INTERFACE is required`.
  - `HEPHAESTUS_PROVISIONING_ADDRESS is required`; `HEPHAESTUS_PROVISIONING_ADDRESS %q: must be an IPv4 address`.
  - `HEPHAESTUS_PROVISIONING_SUBNET is required`; `HEPHAESTUS_PROVISIONING_SUBNET %q: must be an IPv4 CIDR prefix` (stored `Masked()`); `HEPHAESTUS_PROVISIONING_ADDRESS %s is outside HEPHAESTUS_PROVISIONING_SUBNET %s`.
  - `HEPHAESTUS_ADMIN_ADDRESS %q: must be IP:port`; `HEPHAESTUS_ADMIN_ADDRESS %q: must be a specific management IP, not a wildcard`; `HEPHAESTUS_ADMIN_ADDRESS %s is inside the provisioning subnet %s; the admin listener must stay off the VLAN`.
  - `HEPHAESTUS_DATA_DIR %q: must be an absolute path`.
  - `set exactly one of HEPHAESTUS_DATA_KEY or HEPHAESTUS_DATA_KEY_FILE`; `read HEPHAESTUS_DATA_KEY_FILE: %w`; `HEPHAESTUS_DATA_KEY: %w` (wrapping `datakey.Parse`).
  - `set exactly one of HEPHAESTUS_CLOUDFLARE_TOKEN or HEPHAESTUS_CLOUDFLARE_TOKEN_FILE`; `read HEPHAESTUS_CLOUDFLARE_TOKEN_FILE: %w`; file contents are whitespace-trimmed.
  - `HEPHAESTUS_DATABASE_URL: %w` (wrapping `db.ParseURL`); empty means `sqlite:` + `filepath.Join(dataDir, "hephaestus.db")`.
  - `HEPHAESTUS_ACME_DIRECTORY_URL %q: must be an https URL`.
  - `HEPHAESTUS_SHUTDOWN_GRACE must be positive`.
  - `HEPHAESTUS_LOG_FORMAT %q: must be json or text`.

- [ ] **Step 1: Add dependencies**

Run: `rtk proxy go get github.com/spf13/viper@v1.21.0 github.com/go-viper/mapstructure/v2@v2.5.0`
Expected: exit 0.

- [ ] **Step 2: Write the failing tests.** Helper `clearEnv(t)` sets every inherited `HEPHAESTUS_*` variable to `""` with `t.Setenv`; helper `setMinimalEnv(t)` sets `FQDN=hephaestus.example.test`, `PROVISIONING_INTERFACE=vlan20`, `PROVISIONING_ADDRESS=10.10.20.5`, `PROVISIONING_SUBNET=10.10.20.0/24`, `DATA_DIR=<t.TempDir()>`, `DATA_KEY=<fixture>`, `CLOUDFLARE_TOKEN=cf-test-token` (all `HEPHAESTUS_`-prefixed).

```go
func TestLoadConfig_Defaults(t *testing.T)         // AdminAddr == netip.MustParseAddrPort("127.0.0.1:9090"); ACMEDirectoryURL == "https://acme-v02.api.letsencrypt.org/directory"; ShutdownGrace == 60*time.Second; LogFormat "json"; LogLevel slog.LevelInfo; DatabaseURL.Expose() == "sqlite:"+filepath.Join(dir,"hephaestus.db")
func TestLoadConfig_EnvOnlyNestedKey(t *testing.T) // Interface == "vlan20" (nested key with no default, env only: the go-standards §10 canary)
func TestLoadConfig_ReportsEveryProblem(t *testing.T) // only DATA_DIR set → one error containing all six "is required"/"set exactly one" messages for FQDN, interface, address, subnet, data key, Cloudflare token
func TestLoadConfig_Invalid(t *testing.T)          // table; each row = minimal env + one override → error contains:
//  fqdn single label   FQDN=localhost                    → `HEPHAESTUS_FQDN "localhost": must be a fully qualified DNS name`
//  fqdn underscore     FQDN=bad_name.example.test        → `HEPHAESTUS_FQDN "bad_name.example.test": must be a fully qualified DNS name`
//  ipv6 address        PROVISIONING_ADDRESS=fe80::1      → `HEPHAESTUS_PROVISIONING_ADDRESS "fe80::1": must be an IPv4 address`
//  bad prefix          PROVISIONING_SUBNET=10.10.20.0/33 → `HEPHAESTUS_PROVISIONING_SUBNET "10.10.20.0/33": must be an IPv4 CIDR prefix`
//  address outside     PROVISIONING_ADDRESS=10.10.30.5   → `HEPHAESTUS_PROVISIONING_ADDRESS 10.10.30.5 is outside HEPHAESTUS_PROVISIONING_SUBNET 10.10.20.0/24`
//  admin wildcard      ADMIN_ADDRESS=0.0.0.0:9090        → `HEPHAESTUS_ADMIN_ADDRESS "0.0.0.0:9090": must be a specific management IP, not a wildcard`
//  admin on VLAN       ADMIN_ADDRESS=10.10.20.9:9090     → `HEPHAESTUS_ADMIN_ADDRESS 10.10.20.9:9090 is inside the provisioning subnet 10.10.20.0/24; the admin listener must stay off the VLAN`
//  admin no port       ADMIN_ADDRESS=127.0.0.1           → `HEPHAESTUS_ADMIN_ADDRESS "127.0.0.1": must be IP:port`
//  relative data dir   DATA_DIR=relative/dir             → `HEPHAESTUS_DATA_DIR "relative/dir": must be an absolute path`
//  short key           DATA_KEY=AAECAwQFBgcICQoLDA0ODw== → `decoded 16 bytes, want 32`
//  both keys           DATA_KEY_FILE=<file>              → `set exactly one of HEPHAESTUS_DATA_KEY or HEPHAESTUS_DATA_KEY_FILE`
//  missing key file    DATA_KEY="" DATA_KEY_FILE=/nonexistent → `read HEPHAESTUS_DATA_KEY_FILE`
//  mysql url           DATABASE_URL=mysql://x            → `HEPHAESTUS_DATABASE_URL: unsupported scheme`
//  relative sqlite     DATABASE_URL=sqlite:relative.db   → `HEPHAESTUS_DATABASE_URL: sqlite path must be absolute`
//  http acme           ACME_DIRECTORY_URL=http://acme.example.test/dir → `HEPHAESTUS_ACME_DIRECTORY_URL "http://acme.example.test/dir": must be an https URL`
//  zero grace          SHUTDOWN_GRACE=0s                 → `HEPHAESTUS_SHUTDOWN_GRACE must be positive`
//  xml logs            LOG_FORMAT=xml                    → `HEPHAESTUS_LOG_FORMAT "xml": must be json or text`
//  bad level           LOG_LEVEL=loud                    → `decode config`
func TestLoadConfig_FileSecrets(t *testing.T)      // DATA_KEY="" + DATA_KEY_FILE (fixture + "\n"), CLOUDFLARE_TOKEN="" + CLOUDFLARE_TOKEN_FILE ("cf-file-token\n") → CloudflareToken.Expose()=="cf-file-token"; DataKey round-trips Seal/Open
func TestLoadConfig_Normalizes(t *testing.T)       // FQDN=Hephaestus.Example.Test → "hephaestus.example.test"; DATABASE_URL=postgres://u:p@db.example.test:5432/heph accepted unchanged
func TestLoadConfig_SecretsNeverPrinted(t *testing.T) // fmt.Sprintf("%+v", cfg) contains neither "cf-test-token" nor the fixture key
func TestLoadConfig_LogFormatFlag(t *testing.T)       // env LOG_FORMAT=json and flag --log-format=text → LogFormat "text"; flag unset → "json"
// Tests call loadConfig(fs) with fs := pflag.NewFlagSet("t", pflag.ContinueOnError); fs.String("log-format", "", ""); flags set with fs.Parse.
```

- [ ] **Step 3: Run to verify failure**

Run: `rtk proxy go test ./cmd/hephaestus/ -run TestLoadConfig -v`
Expected: FAIL, `undefined: loadConfig`.

- [ ] **Step 4: Implement** `config.go`.

- [ ] **Step 5: Run to verify pass**

Run: `rtk proxy go test ./cmd/hephaestus/ -race -v`
Expected: PASS (all Task 1 and Task 6 tests).

- [ ] **Step 6: Gate and commit**

Run: `DOCKER_HOST=unix://$HOME/.colima/default/docker.sock rtk proxy task ci` — Expected: exit 0.
Run: `rtk proxy git add go.mod go.sum cmd/hephaestus` then `rtk proxy git commit -m "feat: bootstrap configuration from HEPHAESTUS_ env and files" -m "Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"`
Expected: one new commit; `rtk proxy sh -c "git cat-file commit HEAD | grep -c '^gpgsig'"` prints `1`.

---

### Task 7: Fleet domain — states, transitions, identity rules

**Files:**
- Create: `internal/fleet/doc.go`, `state.go`, `transition.go`, `identity.go`, `state_test.go`, `transition_test.go`, `identity_test.go`

**Interfaces:**
- Produces (package `fleet`, all pure):
  - `type State string`: `StateNone = ""` (no record: the From of first contact and adopt-by-UUID, the To of forget), `StateDiscovered = "discovered"`, `StateLocalOnly = "local-only"`, `StateAssigned = "assigned"`, `StateInstalling = "installing"`, `StateInstallUnconfirmed = "install-unconfirmed"`, `StateInstalled = "installed"`, `StateRejected = "rejected"`, `StateOrphaned = "orphaned"`, `StateCordoned = "cordoned"`; `(*State).UnmarshalText` accepts only the nine stored names; `(State).Bound() bool` true for assigned, installing, install-unconfirmed, installed.
  - `type Lifecycle struct { State, Prior, OrphanedFrom State }` — `Prior` is set only while cordoned; `OrphanedFrom` only while orphaned or cordoned-wrapping-orphaned.
  - `type Trigger string`: `TriggerFirstContact "first_contact"`, `TriggerApprove "approve"`, `TriggerAdopt "adopt"`, `TriggerAdoptByUUID "adopt_by_uuid"`, `TriggerMarkLocalOnly "mark_local_only"`, `TriggerUnmarkLocalOnly "unmark_local_only"`, `TriggerReject "reject"`, `TriggerUnreject "unreject"`, `TriggerForget "forget"`, `TriggerUnassign "unassign"`, `TriggerConfigServed "config_served"`, `TriggerNextBoot "next_boot"`, `TriggerAbortInstall "abort_install"`, `TriggerMarkInstalled "mark_installed"`, `TriggerReinstall "reinstall"`, `TriggerHostLeft "host_left"`, `TriggerUnbind "unbind"`, `TriggerRebind "rebind"`, `TriggerCordon "cordon"`, `TriggerUncordon "uncordon"`.
  - `type Guard struct { Junk, HostExists, HostUnbound, ResetConfirmed bool }`.
  - `var ErrInvalidTransition = errors.New("invalid transition")`; `type TransitionError struct { From Lifecycle; Trigger Trigger; Reason string }` — `Error()` = `"<trigger> from <from.State or "none">: <reason>"`, `Unwrap()` = `ErrInvalidTransition`.
  - `func Next(l Lifecycle, t Trigger, g Guard) (Lifecycle, error)` implementing design §5.2's transition table, with reasons `junk_uuid`, `host_not_found`, `host_bound`, `reset_not_confirmed`, `not_bound`, `already_cordoned`, `not_cordoned`, `not_allowed` (any other From/Trigger pair). Guard order for approve, adopt, adopt_by_uuid and rebind: `not_allowed` (wrong From), then `junk_uuid` (not checked for rebind), then `host_not_found` when `!HostExists`, then `host_bound` when `!HostUnbound`; so `host_not_found` wins over `host_bound` for every action that uses both guards.
  - `type UnknownAction string`: `UnknownActionHold "hold"`, `UnknownActionLocal "local"`; `UnmarshalText` rejects others.
  - `type Arch string`: `ArchAMD64 "amd64"`, `ArchARM64 "arm64"`; `func ArchFromBuildArch(s string) Arch` — `"arm64"` → arm64, anything else → amd64 (booter's rule).
  - `func NormalizeUUID(raw string) string` — trims, lowercases, returns `""` unless 8-4-4-4-12 hex; `func JunkPattern(uuid string) bool` — `""`, all-zero, all-`f`; `func NormalizeMAC(raw string) (string, error)` — `net.ParseMAC`, error `must be a 48-bit MAC` unless 6 bytes, returns lowercase colon form.

- [ ] **Step 1: Write the failing tests.** `TestNext_Allowed` is a table; each row asserts the exact resulting `Lifecycle` (omitted fields are `""`). Guards: `ok` = `Guard{HostExists: true, HostUnbound: true}`.

| From | Trigger | Guard | To |
|---|---|---|---|
| {} | first_contact | – | {discovered} |
| {discovered} | approve | ok | {assigned} |
| {discovered} | adopt | ok | {installed} |
| {} | adopt_by_uuid | ok | {installed} |
| {discovered} | mark_local_only | {Junk:true} | {local-only} |
| {local-only} | unmark_local_only | – | {discovered} |
| {discovered} | reject | – | {rejected} |
| {rejected} | unreject | – | {discovered} |
| {rejected}, {discovered}, {local-only} | forget | – | {} |
| {assigned} | unassign | – | {discovered} |
| {assigned} | config_served | – | {installing} |
| {installing} | next_boot | – | {install-unconfirmed} |
| {installing} | abort_install | – | {assigned} |
| {installing}, {install-unconfirmed} | mark_installed | – | {installed} |
| {installed}, {install-unconfirmed} | reinstall | {ResetConfirmed:true} | {assigned} |
| {X} for X in the four bound states | host_left | – | {orphaned, OrphanedFrom:X} |
| {cordoned, Prior:installed} | host_left | – | {cordoned, Prior:orphaned, OrphanedFrom:installed} |
| {orphaned, OrphanedFrom:installed} | unbind | – | {discovered} |
| {orphaned, OrphanedFrom:installed} and {…:install-unconfirmed} | rebind | ok | {installed} |
| {orphaned, OrphanedFrom:installing} and {…:assigned} | rebind | ok | {assigned} |
| {X} for every non-cordoned stored state X; and {orphaned, OrphanedFrom:installed} | cordon | – | {cordoned, Prior:X, OrphanedFrom preserved} |
| {cordoned, Prior:X, OrphanedFrom:Y} | uncordon | – | {X, OrphanedFrom:Y} |

`TestNext_Rejected` table; each row asserts `errors.Is(err, ErrInvalidTransition)`, `errors.As` to `*TransitionError`, and the exact `Reason`. Host-guard rows, for each of approve {discovered}, adopt {discovered}, adopt_by_uuid {} and rebind {orphaned, OrphanedFrom:installed}: `{HostExists:false, HostUnbound:true}` → `host_not_found`; `{HostExists:true, HostUnbound:false}` → `host_bound`; `{HostExists:false, HostUnbound:false}` → `host_not_found`. Other rows: approve {discovered} Junk (host guards ok) → `junk_uuid`; adopt Junk → `junk_uuid`; adopt_by_uuid Junk → `junk_uuid`; adopt_by_uuid from {discovered} → `not_allowed`; reinstall without ResetConfirmed → `reset_not_confirmed`; host_left {discovered} → `not_bound`; host_left {cordoned, Prior:discovered} → `not_bound`; cordon {cordoned,…} → `already_cordoned`; cordon {} → `not_allowed`; uncordon {discovered} → `not_cordoned`; approve {assigned}, forget {assigned}, next_boot {discovered}, config_served {discovered}, unbind {discovered}, first_contact {discovered} → `not_allowed`. Also `TestTransitionError_Message`: `(&TransitionError{From: Lifecycle{}, Trigger: TriggerCordon, Reason: "not_allowed"}).Error() == "cordon from none: not_allowed"`.

```go
func TestState_UnmarshalText(t *testing.T)   // the nine names round-trip; "" and "bogus" error
func TestState_Bound(t *testing.T)           // exactly assigned, installing, install-unconfirmed, installed
func TestUnknownAction_UnmarshalText(t *testing.T) // "hold","local" ok; "exit" errors
func TestArchFromBuildArch(t *testing.T)     // "arm64"→arm64; "x86_64","i386","" → amd64
func TestNormalizeUUID(t *testing.T)         // "4C4C4544-0042-3510-8052-B4C04F4E4D32" → fixture; " "+fixture+" " → fixture; "not-a-uuid" → ""; "" → ""
func TestJunkPattern(t *testing.T)           // "", "00000000-0000-0000-0000-000000000000", "ffffffff-ffff-ffff-ffff-ffffffffffff" → true; fixture → false
func TestNormalizeMAC(t *testing.T)          // "AA-BB-CC-DD-EE-FF", "aa:bb:cc:dd:ee:ff", "aabb.ccdd.eeff" → "aa:bb:cc:dd:ee:ff"; the 20-byte "00:00:00:00:fe:80:00:00:00:00:00:00:02:00:5e:10:00:00:00:01" → error "must be a 48-bit MAC"; "zz" → error
func FuzzNormalizeMAC(f *testing.F)          // seeds: TestNormalizeMAC inputs; property: never panics; on success the result has length 17 and NormalizeMAC(result) == result
func FuzzNormalizeUUID(f *testing.F)         // seeds: TestNormalizeUUID inputs; property: result is "" or 36 lowercase chars, and NormalizeUUID(result) == result
```

- [ ] **Step 2: Run to verify failure**

Run: `rtk proxy go test ./internal/fleet/ -v`
Expected: FAIL, `undefined: Next`.

- [ ] **Step 3: Implement** the signatures above. `Next` is a `switch t` over every `Trigger` (exhaustive) and never touches I/O.

- [ ] **Step 4: Run to verify pass**

Run: `rtk proxy go test ./internal/fleet/ -race -v`
Expected: PASS.
Run: `rtk proxy go test ./internal/fleet/ -run '^$' -fuzz=FuzzNormalizeMAC -fuzztime=30s` then the same with `-fuzz=FuzzNormalizeUUID` — Expected: PASS twice.

- [ ] **Step 5: Gate and commit**

Run: `DOCKER_HOST=unix://$HOME/.colima/default/docker.sock rtk proxy task ci` — Expected: exit 0.
Run: `rtk proxy git add internal/fleet` then `rtk proxy git commit -m "feat: fleet lifecycle transitions and identity rules" -m "Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"`
Expected: one new commit; `rtk proxy sh -c "git cat-file commit HEAD | grep -c '^gpgsig'"` prints `1`.

---

### Task 8: Fleet repository — machines, boots, transitions, audit
Kind: rewrite

**Files:**
- Create: `internal/fleet/machine.go`, `internal/fleet/repository.go`, `internal/fleet/repository_test.go`, `internal/fleet/repository_integration_test.go`, `internal/fleet/main_integration_test.go`
- Modify: `.taskfiles/go.yml`

`.taskfiles/go.yml`: the `DB_PKGS` variable becomes `./internal/db/... ./internal/fleet/...`.

**Interfaces:**
- Consumes: `db.DB`, `dbtest.SQLite`, `dbtest.Postgres`, `dbtest.RunWithPostgres`, Task 7's types.
- Produces (package `fleet`):
  - `type Machine struct` — the bun model of `machines` (an embedded `bun.BaseModel` tagged `bun:"table:machines"`; each quoted string below is the field's `bun:` tag): `ID string "id,pk"`, `UUID string "smbios_uuid"`, `Junk bool "junk"`, `Serial "serial"`, `Product "product"`, `Manufacturer "manufacturer"` (strings), `Arch Arch "arch"`, `State State "state"`, `Prior State "prior_state,nullzero"`, `OrphanedFrom State "orphaned_from,nullzero"`, `ClusterID *string "cluster_id"`, `HostName *string "host_name"`, `CreatedAt time.Time "created_at"`, `LastSeenAt time.Time "last_seen_at"`, `MACs []string "-"` (sorted, filled by `Get` and `Lookup`); `(Machine).Lifecycle() Lifecycle`.
  - `const MaxMachines = 1024`; `const BootEventsKept = 500`; `const MACsKept = 16`; `var ErrFleetFull = errors.New("fleet is full: 1024 machines recorded")`; `var ErrNotFound = errors.New("machine not found")`; `var ErrUnidentifiable = errors.New("machine cannot be identified: no valid UUID and no MAC")`.
  - `type Identity struct { UUID, MAC, Serial, Product string }` (UUID and MAC already normalized; `""` when absent or malformed).
  - `type BootEvent struct { Row int; Action, Reason string }`; `type BootRecord struct { Identity Identity; Arch Arch; MachineID string; Trigger Trigger; Conflict, MarkJunk bool; Event BootEvent; At time.Time }` (`Conflict` = the decision matched row 2).
  - `func NewRepository(d *db.DB) *Repository`.
  - `func (r *Repository) Lookup(ctx context.Context, id Identity) (Machine, bool, error)` — algorithm, exactly: (1) if `JunkPattern(id.UUID)`: when `id.MAC == ""` → not found; else the machine owning `id.MAC` in `machine_macs`. (2) Else load machines with `smbios_uuid = id.UUID`: none → not found; a non-junk one → it; all junk → the one owning `id.MAC`, else the oldest whose serial and product equal `id.Serial`/`id.Product`, else not found.
  - `func (r *Repository) RecordBoot(ctx context.Context, rec BootRecord) (Machine, error)` — one transaction: (a) `MachineID == ""` requires `Trigger == TriggerFirstContact` (else `ErrInvalidTransition`): `JunkPattern(UUID) && MAC == ""` → `ErrUnidentifiable`; count ≥ `MaxMachines` → `ErrFleetFull`; insert `{ID: rand.Text(), Junk: JunkPattern(UUID) || any machine already has this UUID, State: discovered, Created/LastSeen: At}` and the MAC. (b) Known + `Conflict`: when `MarkJunk`, set `junk` on every machine with that UUID and insert audit `{actor "system", action "alert.uuid_conflict", machine_id, detail JSON {"uuid","mac","stored_serial","stored_product","seen_serial","seen_product"}}`; in every case do not attach the MAC, touch `last_seen_at` or apply a trigger. (c) Known otherwise: upsert the MAC with `seen_at = At` (`ON CONFLICT (mac) DO UPDATE SET machine_id = EXCLUDED.machine_id, seen_at = EXCLUDED.seen_at`), then delete the machine's MACs beyond the newest `MACsKept` by `seen_at` (ties broken by `mac`); set `arch` and `last_seen_at`; when `Trigger == TriggerNextBoot` apply `Next` and update the state. (d) Insert the boot event (`from_state`/`to_state` set only when the state changed) unless `Event.Action == "hold"` and the machine's most recent boot event is a hold with the same reason whose `at` is after `At − 1h`; after an insert, delete the machine's boot events beyond the newest `BootEventsKept` (by `id`). Returns the machine as stored. Every MAC upsert anywhere in the repository (here, creation, `RecordReport`) uses the same helper, so the 16-MAC cap always applies.
  - `type TransitionRequest struct { MachineID string; Trigger Trigger; Guard Guard; Actor string; At time.Time }`; `func (r *Repository) Transition(ctx context.Context, req TransitionRequest) (Machine, error)` — one transaction: load (`ErrNotFound`), `req.Guard.Junk = m.Junk`, `Next`; on error no write; `StateNone` deletes the machine (cascade); else update `state`, `prior_state`, `orphaned_from`; insert audit `{actor, action "transition."+trigger, machine_id, detail {"from": <state>, "to": <state or "none">}}`.
  - `func (r *Repository) Get(ctx context.Context, id string) (Machine, error)` — `ErrNotFound` when absent.

- [ ] **Step 1: Write the failing suite** `runRepositorySuite(t *testing.T, open func(testing.TB) *db.DB)` in `repository_test.go`, with `TestRepository_SQLite` there and `TestRepository_Postgres` in `repository_integration_test.go`; `main_integration_test.go` holds the TestMain. Fixed `t0`. Each subtest opens its own database with `open(t)`; a subtest described as `then` or `continuing` first repeats the previous subtest's setup steps. Subtests:
  - `FirstContactCreatesDiscovered`: RecordBoot{Identity{fixture UUID, "aa:bb:cc:00:00:01", "S1", "P1"}, amd64, Trigger first_contact, Event{7, "hold", "awaiting approval"}, At t0} → State discovered, Junk false, MACs ["aa:bb:cc:00:00:01"], CreatedAt.Equal(t0); Lookup with the same identity finds it.
  - `MultiNICSameMachine`: then RecordBoot known with MAC …02, same pair, Trigger "", Event{7,"exit",""} → Get MACs [01, 02]; `SELECT count(*) FROM machines` == 1.
  - `ZeroUUIDKeyedByMAC`: two first contacts with UUID `00000000-0000-0000-0000-000000000000`, MACs …0a and …0b → 2 machines, both Junk; Lookup by each MAC returns its own.
  - `JunkPatternWithoutMACUnidentifiable`: UUID "" MAC "" first contact → `errors.Is(err, ErrUnidentifiable)`; 0 machines.
  - `MarkJunkOnConflict`: machine S1/P1 MAC …01; RecordBoot known Conflict MarkJunk Identity{MAC …09, Serial S2} Event{2,"hold","identity conflict: this UUID was seen with another serial/product"} → Junk true; one audit row `alert.uuid_conflict` for that machine whose detail contains `"seen_serial":"S2"`; `machine_macs` has no …09.
  - `RepeatConflictNoSecondAlert`: own database. Setup, all through RecordBoot: (1) first contact {fixture UUID, …01, S1, P1}, Event{7, "hold", "awaiting approval"}, At t0; (2) known (that machine) Conflict MarkJunk, Identity{fixture UUID, …09, S2, P1}, Event{2, "hold", R}, At t0+10s, where R = `identity conflict: this UUID was seen with another serial/product`; (3) the same as (2) with MarkJunk false at t0+40s. Assert: `SELECT count(*) FROM audit_events WHERE action = 'alert.uuid_conflict'` == 1; `SELECT count(*) FROM boot_events WHERE machine_id = <id> AND decision_row = 2` == 1 (the second hold coalesced); `SELECT count(*) FROM boot_events WHERE machine_id = <id>` == 2.
  - `CloneAfterJunkGetsOwnRecord`: continuing, Lookup{UUID, …09, S2, P1} → not found; first contact → new machine Junk true; Lookup MAC …01 → original; Lookup MAC …09 → clone.
  - `JunkUUIDMatchedByPairOnNewNIC`: continuing, Lookup{UUID, …0c, S1, P1} → the original.
  - `MACMovesToNewMachine`: machine A (UUID U1 `11111111-1111-1111-1111-111111111111`, MAC …20); first contact B (U2 `22222222-2222-2222-2222-222222222222`, MAC …20) → `machine_macs` …20 belongs to B; Get(A).MACs empty.
  - `FleetFull`: bulk-insert 1024 machine rows with bun, then a first contact → `ErrFleetFull`.
  - `NextBootTransition`: machine updated (raw) to installing; RecordBoot known Trigger next_boot Event{6,"exit",""} → State install-unconfirmed; its boot event has from_state "installing", to_state "install-unconfirmed".
  - `HoldCoalescing`: holds with reason A at t0, t0+30s, t0+59m → 1 boot event; t0+61m A → 2; t0+62m reason B → 3.
  - `BootEventsCapped`: one machine; 502 RecordBoot calls with Event{6, "exit", ""} at t0+i·1s → 500 boot events remain and the two oldest ids are gone.
  - `MACsCapped`: one machine; RecordBoot known with 17 distinct MACs …40…50 at t0+i·1s → Get.MACs has 16 entries and lacks …40 (the oldest `seen_at`).
  - `AllStatesStorable`: updating one machine to each of the nine stored states succeeds (proves the CHECK list matches `State`).
  - `TransitionCordonUncordon`: discovered → cordon → State cordoned, Prior discovered; audit action "transition.cordon" detail `{"from":"discovered","to":"cordoned"}`; uncordon → discovered, Prior "".
  - `TransitionForgetDeletes`: forget → Get `ErrNotFound`; no `machine_macs` rows for it; audit "transition.forget" still present with its machine_id and detail `{"from":"discovered","to":"none"}`.
  - `TransitionInvalidNoWrite`: rejected → approve → `errors.Is(err, ErrInvalidTransition)`; no audit rows.
  - `TransitionJunkGuardFromRecord`: junk machine, approve with `Guard{HostExists: true, HostUnbound: true}` → reason `junk_uuid`.
  - `GetNotFound`: Get("nope") → `ErrNotFound`.

- [ ] **Step 2: Run to verify failure**

Run: `rtk proxy go test ./internal/fleet/ -run TestRepository -v`
Expected: FAIL, `undefined: NewRepository`.

- [ ] **Step 3: Implement** `machine.go` and `repository.go` with the semantics above; extend `DB_PKGS`.

- [ ] **Step 4: Run to verify pass on both databases**

Run: `rtk proxy go test ./internal/fleet/ -race -v`
Expected: PASS.
Run: `DOCKER_HOST=unix://$HOME/.colima/default/docker.sock rtk proxy go test -tags integration ./internal/fleet/ -race -run Postgres -v`
Expected: PASS, 20 subtests.

- [ ] **Step 5: Gate and commit**

Run: `DOCKER_HOST=unix://$HOME/.colima/default/docker.sock rtk proxy task ci` — Expected: exit 0; `cover:db` prints nothing.
Run: `rtk proxy git add .taskfiles/go.yml internal/fleet` then `rtk proxy git commit -m "feat: fleet repository with identity resolution, boot events and audited transitions" -m "Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"`
Expected: one new commit; `rtk proxy sh -c "git cat-file commit HEAD | grep -c '^gpgsig'"` prints `1`.

---

### Task 9: Fleet repository — observation, inventory reports, settings
Kind: rewrite

**Files:**
- Modify: `internal/fleet/repository.go`
- Modify: `internal/fleet/repository_test.go`

The new methods live in `repository.go` so the `cover:db` gate (which matches `repository.go`) covers them; the test file gains suite subtests.

**Interfaces:**
- Consumes: Task 8's `Repository`, `Identity`, `Machine`, sentinels.
- Produces (package `fleet`):
  - `const ReportsKept = 10`; `var ErrIdentityConflict = errors.New("identity conflict: stored serial/product differ from the report")`.
  - `type Observation struct { Machine Machine; Created, Alerted bool }`; `func (r *Repository) Observe(ctx context.Context, id Identity, arch Arch, at time.Time) (Observation, error)` — one transaction: `Lookup`; unknown → create exactly as `RecordBoot` (a) does (same unexported helper; same `ErrUnidentifiable`/`ErrFleetFull`) and, in the same transaction, insert audit `{actor "system", action "transition.first_contact", machine_id, detail {"from":"none","to":"discovered"}}` (design §5.2: a transition is applied with its event; `/boot` creations carry a boot event instead), `Created` true; known with serial or product different from `id` → the row-2 treatment of `RecordBoot` (b) (mark the UUID junk and insert the `alert.uuid_conflict` audit row only when the machine is not yet junk; `Alerted` reports whether it did), no other write, return the Observation with `ErrIdentityConflict`; known otherwise → `last_seen_at = at`.
  - `func (r *Repository) LatestReport(ctx context.Context, machineID string) ([]byte, bool, error)`.
  - `func (r *Repository) RecordReport(ctx context.Context, machineID string, facts []byte, macs []string, at time.Time) (bool, error)` — one transaction: upsert every MAC to the machine; `last_seen_at = at`; when `facts` equals the latest report byte for byte return false; else insert `{source "ipxe", facts, received_at at}`, delete all but the newest `ReportsKept` reports for the machine, return true.
  - `func (r *Repository) UnknownAction(ctx context.Context) (UnknownAction, error)` — `settings.unknown_action`, decoded with `UnmarshalText`; a missing row reads as `UnknownActionLocal`.
  - `func (r *Repository) SetUnknownAction(ctx context.Context, a UnknownAction, actor string, at time.Time) error` — upsert plus audit `{actor, action "settings.unknown_action", detail {"from","to"}}`.

- [ ] **Step 1: Add the failing subtests** to `runRepositorySuite`:
  - `ObserveCreates`: unknown identity → Created true, State discovered; exactly one audit row, action `transition.first_contact`, detail `{"from":"none","to":"discovered"}`.
  - `ObserveConflict`: known S1/P1, Observe with S2 → `errors.Is(err, ErrIdentityConflict)`, Alerted true, machine Junk, one `alert.uuid_conflict` row, `last_seen_at` unchanged; a second Observe with S2 → Alerted false, still one alert row.
  - `ObserveTouches`: known, Observe at t0+1m → LastSeenAt.Equal(t0+1m), Created false.
  - `ReportStoredThenDeduped`: RecordReport `{"serial":"S1"}` at t0 → true; same at t0+30s → false; 1 report row; LastSeenAt.Equal(t0+30s).
  - `ReportRetention`: 12 distinct reports (`{"n":1}`…`{"n":12}`) → 10 rows; the remaining facts are `{"n":3}`…`{"n":12}`.
  - `ReportMACsRecorded`: macs ["aa:bb:cc:00:00:31","aa:bb:cc:00:00:32"] → Get.MACs contains both.
  - `LatestReport`: none → ok false; after one report → bytes equal.
  - `UnknownActionDefaultLocal`: fresh database → `UnknownActionLocal`.
  - `SetUnknownAction`: set hold → reads hold; one audit row "settings.unknown_action" detail `{"from":"local","to":"hold"}`.

- [ ] **Step 2: Run to verify failure**

Run: `rtk proxy go test ./internal/fleet/ -run TestRepository -v`
Expected: FAIL, `r.Observe undefined` (compile error).

- [ ] **Step 3: Implement** in `repository.go`.

- [ ] **Step 4: Run to verify pass on both databases**

Run: `rtk proxy go test ./internal/fleet/ -race -v` — Expected: PASS.
Run: `DOCKER_HOST=unix://$HOME/.colima/default/docker.sock rtk proxy go test -tags integration ./internal/fleet/ -race -run Postgres -v` — Expected: PASS, 29 subtests.

- [ ] **Step 5: Gate and commit**

Run: `DOCKER_HOST=unix://$HOME/.colima/default/docker.sock rtk proxy task ci` — Expected: exit 0.
Run: `rtk proxy git add internal/fleet` then `rtk proxy git commit -m "feat: fleet observation, inventory reports and the unknown_action setting" -m "Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"`
Expected: one new commit; `rtk proxy sh -c "git cat-file commit HEAD | grep -c '^gpgsig'"` prints `1`.

---

### Task 10: Boot decision and iPXE scripts

**Files:**
- Create: `internal/boot/doc.go`, `internal/boot/decision.go`, `internal/boot/scripts.go`, `internal/boot/decision_test.go`, `internal/boot/scripts_test.go`

**Interfaces:**
- Consumes: `fleet.Machine`, `fleet.State`, `fleet.Trigger`, `fleet.UnknownAction`, `fleet.Arch`.
- Produces (package `boot`, pure):
  - `type Action string`: `ActionChainTLS "chain_tls"`, `ActionHold "hold"`, `ActionExit "exit"`, `ActionUtility "utility"`, `ActionInstall "install"`.
  - `type Request struct { UUID, MAC, Serial, Product string; Arch fleet.Arch }`.
  - `type Input struct { TLS, Known bool; Machine fleet.Machine; Request Request; UnknownAction fleet.UnknownAction; BootNext string; UtilityReady, AssetsReady, Drift bool }` — the last four are owned by M2/M3.
  - `type Decision struct { Row int; Action Action; Report bool; Reason string; Trigger fleet.Trigger; MarkJunk, Alert, ClearBootNext, NeedsTicket bool; Utility string }`.
  - `func Decide(in Input) Decision` — design §5.2's ten rows in order, first match wins:
    1. `!TLS` → {1, chain_tls}.
    2. `Known` and (`Machine.Serial != Request.Serial` or `Machine.Product != Request.Product`) → {2, hold, Reason `identity conflict: this UUID was seen with another serial/product`, MarkJunk and Alert both `!Machine.Junk`} (a repeat on an already-junk record holds without a second alert).
    3. `BootNext != ""` and `UtilityReady` → {3, utility, Utility BootNext, ClearBootNext}.
    4. cordoned with Prior installed, install-unconfirmed, installing or local-only → {4, exit}.
    5. cordoned (other priors) → {5, hold, `machine is cordoned`}; rejected → `machine is rejected`; orphaned → `machine is orphaned`.
    6. local-only, install-unconfirmed, installed → {6, exit}; installing → {6, exit, Trigger next_boot}.
    7. `!Known` → {7, Report, Trigger first_contact} with hold `awaiting approval` when UnknownAction is hold, else exit; discovered → the same without a Trigger.
    8. assigned and `!AssetsReady` → {8, hold, `boot assets not ready`}.
    9. assigned and `Drift` → {9, hold, `configuration drift: re-approval required`}.
    10. assigned and `Machine.Junk` → {10, hold, `junk SMBIOS UUID: installs are refused`}; assigned → {10, install, NeedsTicket}.
  - `var ErrUnsupportedAction = errors.New("boot: script for this action is not available in M1")`.
  - Script functions; `bootQuery` is the unexported constant `uuid=${uuid}&mac=${net${idx}/mac:hexhyp}&arch=${buildarch}&serial=${serial:uristring}&product=${product:uristring}`:
    - `func StartScript(fqdn string) string` = `"#!ipxe\nchain https://" + fqdn + "/boot?" + bootQuery + "\n"` (also row 1's script).
    - `func ExitScript() string` = `"#!ipxe\nexit\n"`.
    - `func HoldScript(fqdn, reason string) string` = `"#!ipxe\necho hephaestus: holding: " + reason + "\nsleep 30\nchain --replace https://" + fqdn + "/boot?" + bootQuery + "\n"`.
    - `func ReportScript(fqdn, then string) string` = `"#!ipxe\n" + reportBlock(fqdn) + strings.TrimPrefix(then, "#!ipxe\n")`, where `reportBlock` is exactly:
```
params
param uuid ${uuid}
param mac ${net${idx}/mac}
param serial ${serial}
param manufacturer ${manufacturer}
param product ${product}
param cpuvendor ${cpuvendor}
param cpumodel ${cpumodel}
param memsize ${memsize}
param platform ${platform}
param arch ${buildarch}
set nic:int32 0
:hephaestus_nic
isset ${net${nic}/mac} || goto hephaestus_nic_done
param nic${nic}_mac ${net${nic}/mac}
param nic${nic}_chip ${net${nic}/chip}
inc nic
goto hephaestus_nic
:hephaestus_nic_done
imgfetch --name hephaestus-report https://<fqdn>/discover##params || echo hephaestus: discovery report failed
imgfree hephaestus-report ||
```
    - `func Render(d Decision, fqdn string) (string, error)` — chain_tls → StartScript; exit → ExitScript; hold → HoldScript(d.Reason); `d.Report` wraps the result with ReportScript; utility and install → `ErrUnsupportedAction` (M2/M3 render them).
  - Scripts never take request facts as parameters: request facts reach iPXE only as `${…}` variables.

- [ ] **Step 1: Write the failing tests**

`TestDecide` is a table with these rows (fields not named are zero; `known(s)` = Known with Machine{State: s, Serial "S1", Product "P1"} and Request{Serial "S1", Product "P1"}; UnknownAction local unless noted). Each asserts the whole `Decision`.

| Case | Input | Decision |
|---|---|---|
| plain HTTP | TLS false, known(installed) with Request.Serial "S2" | {Row 1, chain_tls} |
| conflict discovered | TLS, known(discovered), Request.Serial "S2" | {2, hold, identity-conflict reason, MarkJunk, Alert} |
| conflict bound | TLS, known(assigned), Request.Product "P2" | same as above |
| conflict repeat | TLS, known(discovered) with Machine.Junk, Request.Serial "S2" | {2, hold, identity-conflict reason} (MarkJunk false, Alert false) |
| conflict beats boot-next | TLS, known(installed), Request.Serial "S2", BootNext "memtest", UtilityReady | Row 2 |
| boot-next ready | TLS, known(installed), BootNext "memtest", UtilityReady | {3, utility, Utility "memtest", ClearBootNext} |
| boot-next not ready | TLS, known(installed), BootNext "memtest" | {6, exit} |
| cordoned exits | TLS, known(cordoned) with Prior each of installed, install-unconfirmed, installing, local-only | {4, exit} |
| cordoned holds | Prior each of discovered, assigned, rejected, orphaned | {5, hold, "machine is cordoned"} |
| rejected | known(rejected) | {5, hold, "machine is rejected"} |
| orphaned | known(orphaned) | {5, hold, "machine is orphaned"} |
| installed-like | known(local-only), known(install-unconfirmed), known(installed) | {6, exit} |
| installing | known(installing) | {6, exit, Trigger next_boot} |
| unknown hold | TLS, Known false, UnknownAction hold | {7, hold, Report, "awaiting approval", Trigger first_contact} |
| unknown local | TLS, Known false | {7, exit, Report, Trigger first_contact} |
| discovered hold | known(discovered), UnknownAction hold | {7, hold, Report, "awaiting approval"} |
| discovered local | known(discovered) | {7, exit, Report} |
| assets missing | known(assigned) | {8, hold, "boot assets not ready"} |
| drift | known(assigned), AssetsReady, Drift | {9, hold, "configuration drift: re-approval required"} |
| junk assigned | known(assigned) with Machine.Junk, AssetsReady | {10, hold, "junk SMBIOS UUID: installs are refused"} |
| install | known(assigned), AssetsReady | {10, install, NeedsTicket} |

```go
func TestStartScript(t *testing.T) // StartScript("hephaestus.example.test") == "#!ipxe\nchain https://hephaestus.example.test/boot?uuid=${uuid}&mac=${net${idx}/mac:hexhyp}&arch=${buildarch}&serial=${serial:uristring}&product=${product:uristring}\n"
func TestHoldScript(t *testing.T)  // == "#!ipxe\necho hephaestus: holding: awaiting approval\nsleep 30\nchain --replace https://hephaestus.example.test/boot?<bootQuery>\n"
func TestExitScript(t *testing.T)  // == "#!ipxe\nexit\n"
func TestReportScript(t *testing.T) // ReportScript(fqdn, HoldScript(fqdn,"awaiting approval")): HasPrefix "#!ipxe\nparams\nparam uuid ${uuid}\n"; contains "imgfetch --name hephaestus-report https://hephaestus.example.test/discover##params"; HasSuffix "echo hephaestus: holding: awaiting approval\nsleep 30\nchain --replace https://hephaestus.example.test/boot?"+bootQuery+"\n"; strings.Count(s, "#!ipxe") == 1
func TestRender(t *testing.T)      // chain_tls, exit, hold, Report+exit map to the functions above; utility and install → errors.Is(err, ErrUnsupportedAction)
```

- [ ] **Step 2: Run to verify failure**

Run: `rtk proxy go test ./internal/boot/ -v`
Expected: FAIL, `undefined: Decide`.

- [ ] **Step 3: Implement** the signatures above.

- [ ] **Step 4: Run to verify pass**

Run: `rtk proxy go test ./internal/boot/ -race -v`
Expected: PASS.

- [ ] **Step 5: Gate and commit**

Run: `DOCKER_HOST=unix://$HOME/.colima/default/docker.sock rtk proxy task ci` — Expected: exit 0.
Run: `rtk proxy git add internal/boot` then `rtk proxy git commit -m "feat: the /boot decision function and iPXE scripts" -m "Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"`
Expected: one new commit; `rtk proxy sh -c "git cat-file commit HEAD | grep -c '^gpgsig'"` prints `1`.

---

### Task 11: iPXE build and the embedded binaries
Kind: rewrite

**Files:**
- Create: `build/ipxe/Dockerfile`, `build/ipxe/build.sh`, `build/ipxe/embed.ipxe`, `build/ipxe/roots.txt`, `build/ipxe/config/general.h`, `build/ipxe/config/settings.h`, `build/ipxe/config/crypto.h`
- Create (build outputs, committed): `internal/boot/ipxe/bin/undionly.kpxe`, `snp-amd64.efi`, `snp-arm64.efi`, `SHA256SUMS`, `BUILDINFO`, `COPYING`, `COPYING.GPLv2`, `COPYING.UBDL`
- Create: `internal/boot/ipxe/doc.go`, `internal/boot/ipxe/ipxe.go`, `internal/boot/ipxe/ipxe_test.go`
- Modify: `taskfile.yml`

`taskfile.yml` gains `ipxe:image`, `ipxe:build` and `ipxe:build:testca`.

**Interfaces:**
- Produces (package `ipxe`): constants `TFTPBIOS = "undionly.kpxe"`, `TFTPAMD64 = "snp.efi"`, `TFTPARM64 = "snp-arm64.efi"`, `HTTPAMD64 = "amd64/snp.efi"`, `HTTPARM64 = "arm64/snp.efi"` (the names the vendored proxy hands out; HTTP names are relative to `/tftp/`); `func Files() (map[string][]byte, error)` returning exactly those five keys (`snp.efi` and `amd64/snp.efi` → `bin/snp-amd64.efi`; `snp-arm64.efi` and `arm64/snp.efi` → `bin/snp-arm64.efi`; `undionly.kpxe` → `bin/undionly.kpxe`) from `//go:embed bin/undionly.kpxe bin/snp-amd64.efi bin/snp-arm64.efi`.

**Exact file contents.**

`build/ipxe/roots.txt` (name, URL, file sha256, DER sha256 fingerprint; verified 2026-11-14):
```
isrgrootx1.pem https://letsencrypt.org/certs/isrgrootx1.pem 22b557a27055b33606b6559f37703928d3e4ad79f110b407d04986e1843543d1 96bcec06264976f37460779acf28c5a7cfe8a3c0aae11a8ffcee05c0bddf08c6
isrg-root-x2.pem https://letsencrypt.org/certs/isrg-root-x2.pem a13d881e11fe6df181b53841f9fa738a2d7ca9ae7be3d53c866f722b4242b013 69729b8e15a86efc177a57afb7171dfc64add28c2fca8cf1507e34453ccb1470
root-yr.pem https://letsencrypt.org/certs/gen-y/root-yr.pem d8a34bfb5df6b4a0592b0a9cd6b3b9a9335bd431cb2ac32243ab298cba24cbf2 e57b7e6f150c419102e8d5c055729ff967b9d1a829bf00cec89ca604ebf4a86f
root-ye.pem https://letsencrypt.org/certs/gen-y/root-ye.pem b8471c8049835b4097069ae3d43ea8235b510c0aa2699dedaaab9bc10be3d2c4 e14ffcad5b0025731006caa43a121a22d8e9700f4fb9cf852f02a708aa5d5666
```
`build/ipxe/config/general.h`:
```c
/* hephaestus (design §5.10): HTTPS, request parameters and NTP on every platform, BIOS included. */
#undef DOWNLOAD_PROTO_HTTPS
#define DOWNLOAD_PROTO_HTTPS
#undef PARAM_CMD
#define PARAM_CMD
#undef NTP_CMD
#define NTP_CMD
```
`build/ipxe/config/settings.h`:
```c
/* hephaestus (design §5.10): ${memsize} from the memory map. */
#undef MEMMAP_SETTINGS
#define MEMMAP_SETTINGS
```
`build/ipxe/config/crypto.h`:
```c
/* hephaestus (design §5.10): no cross-signed certificate download (an empty value disables it), no OCSP. */
#undef CROSSCERT
#define CROSSCERT ""
#undef OCSP_CHECK
```
`build/ipxe/embed.ipxe` (design §5.10 static chain script):
```
#!ipxe
prompt --key 0x02 --timeout 2000 Press Ctrl-B for the iPXE command line... && shell ||
ifstat
set attempts:int32 10
set x:int32 0
:retry_loop
set idx:int32 0
:loop
isset ${net${idx}/mac} || goto exhausted
ifclose
iflinkwait --timeout 5000 net${idx} || goto next_iface
dhcp net${idx} || goto next_iface
isset ${ntp} && ntp ${ntp} ||
chain http://${proxydhcp/next-server}/start?uuid=${uuid}&mac=${net${idx}/mac:hexhyp}&arch=${buildarch} || goto next_iface
exit
:next_iface
inc idx && goto loop
:exhausted
echo hephaestus: no boot server answered on any interface
iseq ${x} ${attempts} && goto fail ||
echo hephaestus: retrying in 5 seconds
sleep 5
inc x
goto retry_loop
:fail
echo hephaestus: giving up after ${attempts} attempts, returning to the firmware
exit
```
`build/ipxe/Dockerfile`:
```dockerfile
FROM debian:trixie-slim@sha256:a72b4433de1cf7d6bae0c4cee74d0547f58601a643b3375f070c51600d63b6b2

ARG IPXE_COMMIT=9261ebe38af0c0b38ac332adeec125a518845013
ARG IPXE_TLS13_ANCESTOR=a040f95796505c86619905eb27758aebd6b0ab87
ENV DEBIAN_SNAPSHOT=20261104T212339Z IPXE_COMMIT=${IPXE_COMMIT} IPXE_TLS13_ANCESTOR=${IPXE_TLS13_ANCESTOR}

# hadolint ignore=DL3008
RUN set -eux; \
    rm -f /etc/apt/sources.list.d/debian.sources; \
    printf 'Types: deb\nURIs: http://snapshot.debian.org/archive/debian/%s\nSuites: trixie trixie-updates\nComponents: main\nSigned-By: /usr/share/keyrings/debian-archive-keyring.pgp\n\nTypes: deb\nURIs: http://snapshot.debian.org/archive/debian-security/%s\nSuites: trixie-security\nComponents: main\nSigned-By: /usr/share/keyrings/debian-archive-keyring.pgp\n' "$DEBIAN_SNAPSHOT" "$DEBIAN_SNAPSHOT" > /etc/apt/sources.list.d/snapshot.sources; \
    apt-get -o Acquire::Check-Valid-Until=false update; \
    apt-get install -y --no-install-recommends ca-certificates git make gcc libc6-dev binutils gcc-x86-64-linux-gnu binutils-x86-64-linux-gnu liblzma-dev perl openssl curl; \
    rm -rf /var/lib/apt/lists/*

RUN set -eux; \
    git clone https://github.com/ipxe/ipxe /ipxe; \
    git -C /ipxe checkout -q "$IPXE_COMMIT"; \
    git -C /ipxe merge-base --is-ancestor "$IPXE_TLS13_ANCESTOR" HEAD
```
(The DL3008 ignore is justified: package versions are pinned by the snapshot timestamp. The apt sources use plain `http://snapshot…` because the slim image has no CA bundle before `ca-certificates` installs; apt verifies signatures.)

`build/ipxe/build.sh` — POSIX `#!/bin/sh`, `set -eu`, under 120 lines, run inside the builder as root with the repository at `/src`. Usage `build.sh shipped` or `build.sh testca <repo-relative CA PEM>`. It does, in order:
1. `cd /ipxe`; fail unless `git rev-parse HEAD` equals `$IPXE_COMMIT`; re-run `git merge-base --is-ancestor "$IPXE_TLS13_ANCESTOR" HEAD`.
2. `shipped`: `mkdir -p /tmp/roots`; for each `roots.txt` line, `curl -fsSL <url> -o /tmp/roots/<name>`, fail with `root <name>: file sha256 mismatch` unless `sha256sum` matches, fail with `root <name>: fingerprint mismatch` unless `openssl x509 -in <file> -outform DER | sha256sum` matches; `CERTS` = the four paths comma-joined in file order and `EXPECT` = the four fingerprints concatenated in that order. `testca`: `CERTS=/src/<pem>`, `EXPECT` = its DER fingerprint.
3. `cp /src/build/ipxe/config/general.h /src/build/ipxe/config/settings.h /src/build/ipxe/config/crypto.h src/config/local/`; `export SOURCE_DATE_EPOCH="$(git log -1 --format=%ct)"`.
4. `make -C src -j"$(nproc)" CROSS=x86_64-linux-gnu- bin-x86_64-pcbios/undionly.kpxe bin-x86_64-efi/snp.efi EMBED=/src/build/ipxe/embed.ipxe CERT="$CERTS" TRUST="$CERTS"` then `make -C src -j"$(nproc)" bin-arm64-efi/snp.efi EMBED=/src/build/ipxe/embed.ipxe CERT="$CERTS" TRUST="$CERTS"`; on failure print the last 40 log lines and exit 1.
5. For each `.tmp` (`bin-x86_64-pcbios/undionly.kpxe.tmp`, `bin-x86_64-efi/snp.efi.tmp`, `bin-arm64-efi/snp.efi.tmp`): the `nm -S` line ending ` fingerprints` must show size `0000000000000080` (shipped) or `0000000000000020` (testca), and `od -An -v -tx1 <tmp> | tr -d ' \n'` must contain `$EXPECT`; else exit 1 with `trust check failed for <tmp>`.
6. Output directory: shipped → `/src/internal/boot/ipxe/bin`; testca → `/src/build/ipxe/out/testca`. Copy the three binaries as `undionly.kpxe`, `snp-amd64.efi`, `snp-arm64.efi`. Shipped only: copy `/ipxe/COPYING`, `/ipxe/COPYING.GPLv2`, `/ipxe/COPYING.UBDL`; write `SHA256SUMS` with `sha256sum undionly.kpxe snp-amd64.efi snp-arm64.efi` run inside the output directory; write `BUILDINFO` with exactly these lines in this order:
```
ipxe-commit: <git rev-parse HEAD>
ipxe-ancestor: <IPXE_TLS13_ANCESTOR>
source-date-epoch: <SOURCE_DATE_EPOCH>
debian-snapshot: <DEBIAN_SNAPSHOT>
gcc: <gcc --version | head -1>
x86_64-gcc: <x86_64-linux-gnu-gcc --version | head -1>
embed-sha256: <sha256 of build/ipxe/embed.ipxe>
config-sha256: <sha256> general.h
config-sha256: <sha256> settings.h
config-sha256: <sha256> crypto.h
trust undionly.kpxe <EXPECT>
trust snp-amd64.efi <EXPECT>
trust snp-arm64.efi <EXPECT>
```
7. When `HOST_UID` and `HOST_GID` are set, `chown -R "$HOST_UID:$HOST_GID"` the output directory.

`taskfile.yml` additions (same `tasks:` map):
```yaml
  ipxe:image:
    desc: Build the pinned iPXE builder image (linux/arm64)
    cmds:
      - docker build --platform linux/arm64 -t hephaestus-ipxe-builder:local build/ipxe
  ipxe:build:
    desc: Rebuild internal/boot/ipxe/bin from iPXE 9261ebe (reproducible; needs DOCKER_HOST)
    deps: [ipxe:image]
    cmds:
      - docker run --rm --platform linux/arm64 -e HOST_UID="$(id -u)" -e HOST_GID="$(id -g)" -v "{{.ROOT_DIR}}:/src" hephaestus-ipxe-builder:local sh /src/build/ipxe/build.sh shipped
  ipxe:build:testca:
    desc: Build the e2e-only test-CA variant into build/ipxe/out/testca (never embedded)
    deps: [ipxe:image]
    requires:
      vars: [TEST_CA]
    cmds:
      - docker run --rm --platform linux/arm64 -e HOST_UID="$(id -u)" -e HOST_GID="$(id -g)" -v "{{.ROOT_DIR}}:/src" hephaestus-ipxe-builder:local sh /src/build/ipxe/build.sh testca "{{.TEST_CA}}"
```

- [ ] **Step 1: Write the build inputs** above and check them.

Run: `rtk proxy sh -c "hadolint build/ipxe/Dockerfile && echo hadolint-ok"` — Expected: `hadolint-ok`.
Run: `DOCKER_HOST=unix://$HOME/.colima/default/docker.sock docker info --format '{{.ServerVersion}}'` — Expected: a version string; otherwise BLOCKED per Global Constraints.

- [ ] **Step 2: Build the shipped binaries**

Run: `DOCKER_HOST=unix://$HOME/.colima/default/docker.sock rtk proxy task ipxe:build`
Expected: exit 0 in about 2 minutes; `internal/boot/ipxe/bin/` holds the eight files; `undionly.kpxe` is 100–130 KB (116,970 bytes in the planning build), each `.efi` 280–380 KB (315,392 and 345,600 bytes in the planning build).

- [ ] **Step 3: Write the failing tests** (`ipxe_test.go`). The ISRG oracle is a test literal, independent of the build inputs: `isrg := []string{"96bcec06264976f37460779acf28c5a7cfe8a3c0aae11a8ffcee05c0bddf08c6", "69729b8e15a86efc177a57afb7171dfc64add28c2fca8cf1507e34453ccb1470", "e57b7e6f150c419102e8d5c055729ff967b9d1a829bf00cec89ca604ebf4a86f", "e14ffcad5b0025731006caa43a121a22d8e9700f4fb9cf852f02a708aa5d5666"}`; `ipxeDefaultCA := "9faf717b7f8ca2f93c256c79f8ac5591895d66d1ff3bee6397a70d29c65eed1a"`.

```go
func TestBinariesMatchSHA256SUMS(t *testing.T) // bin/SHA256SUMS has exactly 3 lines "<hex>  <name>" for undionly.kpxe, snp-amd64.efi, snp-arm64.efi; each equals sha256 of the embedded file
func TestRootsTxtIsISRG(t *testing.T)          // the 4th column of ../../../build/ipxe/roots.txt, in order, == isrg
func TestEFITrustIsISRG(t *testing.T)          // for both .efi: bytes.Count(bin, decodeHex(strings.Join(isrg,""))) == 1; !bytes.Contains(bin, decodeHex(ipxeDefaultCA))
func TestBuildInfoTrust(t *testing.T)          // BUILDINFO has "trust <name> <hex>" for all three binaries with hex == strings.Join(isrg,"")
func TestBuildInfoPins(t *testing.T)           // BUILDINFO "ipxe-commit: X" equals ARG IPXE_COMMIT in ../../../build/ipxe/Dockerfile; "ipxe-ancestor: Y" equals ARG IPXE_TLS13_ANCESTOR there; "embed-sha256:" equals sha256 of ../../../build/ipxe/embed.ipxe
func TestEmbeddedScriptInEFI(t *testing.T)     // both .efi contain the bytes of ../../../build/ipxe/embed.ipxe
func TestFiles(t *testing.T)                   // Files() keys == {undionly.kpxe, snp.efi, snp-arm64.efi, amd64/snp.efi, arm64/snp.efi}; snp.efi and amd64/snp.efi share bytes; snp-arm64.efi and arm64/snp.efi share bytes; none empty
```
Run: `rtk proxy go test ./internal/boot/ipxe/ -v` — Expected: FAIL, `undefined: Files`.

- [ ] **Step 4: Implement `ipxe.go`.**

Run: `rtk proxy go test ./internal/boot/ipxe/ -race -v` — Expected: PASS, 7 tests.

- [ ] **Step 5: Prove reproducibility**

Run: `mkdir -p build/ipxe/.work && cp internal/boot/ipxe/bin/SHA256SUMS build/ipxe/.work/SHA256SUMS.first`
Run: `DOCKER_HOST=unix://$HOME/.colima/default/docker.sock rtk proxy task ipxe:build` then `cmp build/ipxe/.work/SHA256SUMS.first internal/boot/ipxe/bin/SHA256SUMS && echo reproducible`
Expected: `reproducible` (the rebuild is byte-identical). If it differs, compare `BUILDINFO`, fix the non-determinism in `build.sh`, and repeat Steps 2–5.

- [ ] **Step 6: Build the test-CA variant once and confirm it stays out of the embedded set**

Run: `openssl req -x509 -newkey rsa:2048 -nodes -keyout build/ipxe/.work/test-ca.key -out build/ipxe/.work/test-ca.pem -days 1 -subj /CN=hephaestus-test-ca`
Run: `DOCKER_HOST=unix://$HOME/.colima/default/docker.sock rtk proxy task ipxe:build:testca TEST_CA=build/ipxe/.work/test-ca.pem` then `cmp build/ipxe/.work/SHA256SUMS.first internal/boot/ipxe/bin/SHA256SUMS && echo embedded-untouched` then `rtk proxy sh -c "git status --porcelain build/ipxe/out build/ipxe/.work | wc -l | tr -d ' '"`
Expected: exit 0; `build/ipxe/out/testca/` holds three binaries; `embedded-untouched`; `0` (both directories are gitignored).

- [ ] **Step 7: Gate**

Run: `rtk proxy sh -c "task --list-all >/dev/null && echo taskfile-ok"` — Expected: `taskfile-ok`.
Run: `DOCKER_HOST=unix://$HOME/.colima/default/docker.sock rtk proxy task ci` — Expected: exit 0; hadolint now lints `build/ipxe/Dockerfile`.

- [ ] **Step 8: Commit**

Run: `rtk proxy git add build/ipxe internal/boot/ipxe taskfile.yml` then `rtk proxy git commit -m "feat: reproducible iPXE 9261ebe build with ISRG trust and the embedded chain script" -m "Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"`
Expected: one new commit; `rtk proxy sh -c "git cat-file commit HEAD | grep -c '^gpgsig'"` prints `1`.

- [ ] **Step 9: Commit any later fix separately**

If anything in this task changes after Step 8 (a critic fix, a rerun of Step 7), rerun Step 7 and commit only those files with `-m "fix(ipxe): <what>"` and the trailer. Expected: the same `gpgsig` check prints `1`.

---

### Task 12: Vendored proxyDHCP and TFTP
Kind: rewrite

**Files:**
- Create: `internal/third_party/omniprovider/LICENSE` (the provider's `LICENSE`, MPL-2.0, verbatim), `internal/third_party/omniprovider/VENDORED.md`
- Create: `internal/third_party/omniprovider/dhcp/dhcp.go`, `proxy.go`, `export_test.go`, `proxy_test.go`, `contract_test.go`
- Create: `internal/third_party/omniprovider/tftp/tftp_server.go`
- Modify: `go.mod`
- Modify: `go.sum`

The only change to existing files is dependency additions (`Kind: rewrite` per the go.mod ruling).

**Source:** `git clone --depth 1 --branch v0.15.0 https://github.com/siderolabs/omni-infra-provider-bare-metal` into a fresh empty scratch directory; commit `13d64d3ec8e1a4f90f50af160380b086030f2343`. Copy `pkg/dhcp/{dhcp.go,proxy.go,export_test.go,proxy_test.go}` and `pkg/tftp/tftp_server.go`. Do not copy `pkg/dhcp/boot_contract_test.go` (it imports provider-internal packages); `contract_test.go` replaces it.

**Changes, and only these:** keep each MPL header; directly under it add `// hephaestus: vendored from github.com/siderolabs/omni-infra-provider-bare-metal v0.15.0 (13d64d3ec8e1a4f90f50af160380b086030f2343), <original path>. Changes: <list>.`; replace `*zap.Logger` with `*slog.Logger` (`zap.String(k,v)` → `slog.String(k,v)`, `zap.Int` → `slog.Int`, `zap.Int64` → `slog.Int64`, `zap.Error(err)` → `slog.Any("error", err)`, `logger.With(zap…)` → `logger.With(slog…)`, keeping each call's level); replace the `xslices.Map(...)` call in `validateDHCP` with an equivalent loop (dropping `github.com/siderolabs/gen`); in `proxy_test.go` rewrite the import of the provider's own `github.com/siderolabs/omni-infra-provider-bare-metal/pkg/dhcp` to `go.jacaudi.dev/hephaestus/internal/third_party/omniprovider/dhcp`. Tests stay on testify. Upstream log levels stay as they are (Global Constraints exemption).

**Interfaces:**
- Produces: `dhcp.NewProxy(apiAdvertiseAddress string, apiPort int, proxyIfaceOrIP string, disableBroadcastListener bool, logger *slog.Logger) *dhcp.Proxy` with `Run(ctx) error`; `tftp.NewServer(listenAddress string, files map[string][]byte, logger *slog.Logger) *tftp.Server` with `Run(ctx) error` (binds port 69).
- `VENDORED.md` records: source repository, tag, commit, licence (MPL-2.0), the five copied files with their original paths, the change list above, and that lint skips `internal/third_party/`.

- [ ] **Step 1: Add dependencies**

Run: `rtk proxy go get github.com/insomniacslk/dhcp@v0.0.0-20261014062924-c6bb46ad3f3a github.com/pin/tftp/v3@v3.2.0 golang.org/x/sync@v0.25.0 github.com/stretchr/testify@v1.12.1` — Expected: exit 0.

- [ ] **Step 2: Write the failing contract test** `contract_test.go` (`package dhcp_test`, testify):

```go
func TestAdvertisedBootFilesAreServed(t *testing.T)
// files, err := ipxe.Files(); require.NoError
// offer(fw) = dhcp.OfferDHCP(PXE REQUEST, "10.10.20.5", 80, fw, dhcp.Port4011)
// FirmwareX86PC, FirmwareX86EFI, FirmwareARMEFI: resp.BootFileName is a key of files
// FirmwareX86Ipxe: strings.TrimPrefix(url.Path, "/") of BootFileNameOption() is a key of files
// FirmwareX86HTTP, FirmwareARMHTTP: URL host "10.10.20.5:80", path "/tftp/" + key with key in files
```
Run: `rtk proxy go test ./internal/third_party/omniprovider/... -v` — Expected: FAIL (package not yet present).

- [ ] **Step 3: Copy and adapt** the files per the change list.

- [ ] **Step 4: Run to verify pass**

Run: `rtk proxy go test ./internal/third_party/omniprovider/... -race -v`
Expected: PASS: `TestIsBootDHCP`, `TestOfferDHCP` (upstream) and `TestAdvertisedBootFilesAreServed`.
Run: `rtk proxy sh -c "grep -rn --include='*.go' '\"go.uber.org/zap\|\"github.com/siderolabs/gen\|\"github.com/siderolabs/omni-infra-provider-bare-metal' internal/third_party || echo clean"` — Expected: `clean` (import lines only; the provenance comments mention the provider by design, without a leading quote).
Run: `rtk proxy task go:cross` — Expected: exit 0 (linux and darwin, amd64 and arm64).

- [ ] **Step 5: Gate and commit**

Run: `DOCKER_HOST=unix://$HOME/.colima/default/docker.sock rtk proxy task ci` — Expected: exit 0.
Run: `rtk proxy git add go.mod go.sum internal/third_party` then `rtk proxy git commit -m "feat: vendor the Omni bare-metal provider's proxyDHCP and TFTP (v0.15.0, MPL-2.0)" -m "Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"`
Expected: one new commit; `rtk proxy sh -c "git cat-file commit HEAD | grep -c '^gpgsig'"` prints `1`.

---

### Task 13: Boot HTTP routes — guard, /start, /tftp, /boot

**Files:**
- Create: `internal/boot/guard.go`, `internal/boot/handlers.go`, `internal/boot/guard_test.go`, `internal/boot/handlers_test.go`

**Interfaces:**
- Consumes: `fleet.Repository` (`Lookup`, `RecordBoot`, `UnknownAction`), `fleet.NormalizeUUID`, `fleet.NormalizeMAC`, `fleet.ArchFromBuildArch`, Task 10's `Decide`/`Render`/`StartScript`, `telemetry.AddAttrs`, `ipxe.HTTPAMD64`/`ipxe.HTTPARM64`, `dbtest.SQLite`, `telemetry.NewMetrics` (tests).
- Produces (package `boot`):
  - `func RequireSubnet(subnet netip.Prefix, next http.Handler) http.Handler` — parses `r.RemoteAddr` with `netip.ParseAddrPort`, `Unmap()`s it, and answers `403` with an empty body unless the subnet contains it.
  - `type Option func(*Handlers)`; `func WithClock(now func() time.Time) Option`; `func WithMeter(m metric.Meter) Option` (default `noop.NewMeterProvider().Meter("")` from `go.opentelemetry.io/otel/metric/noop`).
  - `func NewHandlers(repo *fleet.Repository, fqdn string, subnet netip.Prefix, files map[string][]byte, opts ...Option) (*Handlers, error)` — creates the counters `hephaestus.boot.decisions` (attrs `decision_row` int, `action` string) and `hephaestus.alerts` (attr `kind`).
  - `func (h *Handlers) PlainHTTP() http.Handler` — `RequireSubnet` over a mux with `GET /start`, `GET /boot`, `GET /tftp/{arch}/snp.efi`.
  - `func (h *Handlers) TLS() http.Handler` — `RequireSubnet` over a mux with `GET /boot` (Task 14 adds `POST /discover`).
  - `const maxValueBytes = 256` (one constant, reused by Task 14).
  - `func parseBootQuery(q url.Values) (Request, error)` — the `/boot` query rules below; `errValueTooLong` when any of `uuid`, `mac`, `serial`, `product`, `arch` exceeds `maxValueBytes`.
  - `func m1Fallback(d Decision) Decision` — `NeedsTicket` → `{Row: d.Row, Action: hold, Reason: "ticket minting not available (M3)"}`; `ActionUtility` → `{Row: d.Row, Action: hold, Reason: "utility boot not available (M2)"}` with `ClearBootNext` false; anything else unchanged.
- Route behaviour:
  - Script responses: `Content-Type: text/plain; charset=utf-8`; per-route write deadline `http.NewResponseController(w).SetWriteDeadline(now+10s)`, ignoring only `http.ErrNotSupported`.
  - `/start`: always `StartScript(fqdn)`; reads nothing from the request.
  - `/boot` query (`parseBootQuery`): `uuid` → `NormalizeUUID`; `mac` → `NormalizeMAC` (an error means `""`); `serial`, `product`; `arch` → `ArchFromBuildArch`. Any of these values longer than `maxValueBytes` → `400` with an empty body. Plain HTTP (`r.TLS == nil`): `Decide(Input{TLS: false})` and its script, no DB access. TLS: `Lookup` → `UnknownAction` → `Decide` (M1 passes `BootNext ""` and `UtilityReady`, `AssetsReady`, `Drift` false) → `m1Fallback` → `RecordBoot{Identity, Arch, MachineID (when known), Trigger, Conflict: d.Row == 2, MarkJunk, Event{Row, string(Action), Reason}, At now}` → `Render`. `ErrFleetFull` and `ErrUnidentifiable` from `RecordBoot` are not errors: the script is still served and the wide event gets `fleet_full=true` or `unidentified=true`. Any other repository error → `500` body `internal error\n` with the error in the wide event (`error` key). Counters: decisions +1 per rendered decision; alerts `{kind: uuid_conflict}` +1 when `Alert`. Wide event attrs: `decision_row`, `action`, `hold_reason` (when hold), `machine_id` (when known), `state` (the machine's state as `RecordBoot` returned it, i.e. after the transition).
  - `/tftp/{arch}/snp.efi`: `arch` must be `amd64` or `arm64` (else 404); serves `files[arch+"/snp.efi"]` with `http.ServeContent(w, r, "snp.efi", time.Time{}, bytes.NewReader(b))` and `Content-Type: application/octet-stream`, through an idle-write wrapper that calls `SetWriteDeadline(now+30s)` before every `Write` (implements `Unwrap`).

- [ ] **Step 1: Write the failing tests.** Requests use `httptest.NewRequestWithContext` with `RemoteAddr = "10.10.20.50:40000"`; TLS requests set `req.TLS = &tls.ConnectionState{}`; repository from `dbtest.SQLite(t)`; files `{"amd64/snp.efi": []byte("amd64-efi"), "arm64/snp.efi": []byte("arm64-efi")}`; boot query `q := "uuid=4c4c4544-0042-3510-8052-b4c04f4e4d32&mac=aa-bb-cc-00-00-01&arch=x86_64&serial=S1&product=P1"`.

```go
func TestRequireSubnet(t *testing.T)            // RemoteAddr → status: "10.10.20.50:1"→200; "10.10.30.5:1"→403; "[::ffff:10.10.20.50]:1"→200; "[2001:db8::1]:1"→403; "garbage"→403
func TestStart_StaticScript(t *testing.T)       // GET /start?uuid=x&serial=%0Aevil → 200, body == StartScript("hephaestus.example.test"), no "evil"
func TestBootPlain_ChainsToTLS(t *testing.T)    // PlainHTTP GET /boot?q → body == StartScript(fqdn); 0 machines
func TestBoot_UnknownLocalReportsAndExits(t *testing.T) // TLS GET /boot?q → body HasPrefix "#!ipxe\nparams\n", HasSuffix "exit\n"; machine discovered with MAC aa:bb:cc:00:00:01; one boot event decision_row 7, action "exit"
func TestBoot_UnknownHoldReportsAndHolds(t *testing.T)  // SetUnknownAction(hold) first → body contains "echo hephaestus: holding: awaiting approval"
func TestBoot_RequestTextNeverEchoed(t *testing.T)      // serial "S1%0A%23!ipxe%0Achain%20http://evil.example", product "%24%7Bevil%7D" → body contains neither "evil" nor "chain http://evil"
func TestBoot_IdentityConflictHolds(t *testing.T)       // first boot S1; second boot serial S2 → body contains "holding: identity conflict"; machine Junk; one "alert.uuid_conflict" audit row; a third boot with S2 → still one alert row
func TestBoot_AssignedHoldsOnAssets(t *testing.T)       // machine set to assigned (raw update) → body contains "holding: boot assets not ready"
func TestBoot_InstalledExits(t *testing.T)              // installed → body == "#!ipxe\nexit\n"
func TestBoot_InstallingBecomesUnconfirmed(t *testing.T) // installing → exit; Get → install-unconfirmed
func TestBoot_OversizeParamRejected(t *testing.T)       // serial of 300 "a" → 400; 0 machines
func TestBoot_MetricsCount(t *testing.T)                // WithMeter(metrics.Meter()); one unknown-local boot → metrics body contains `hephaestus_boot_decisions_total{action="exit",decision_row="7"`
func TestM1Fallback(t *testing.T)                       // {10, install, NeedsTicket} → {10, hold, "ticket minting not available (M3)"}; {3, utility, Utility "memtest", ClearBootNext} → {3, hold, "utility boot not available (M2)"}; {6, exit} unchanged
func TestTFTPRoute(t *testing.T)                        // GET /tftp/amd64/snp.efi → 200 "amd64-efi", Content-Type application/octet-stream; /tftp/riscv64/snp.efi → 404; Range "bytes=0-3" → 206, body "amd6"
func TestTFTPRoute_DeadlineOnRealConn(t *testing.T)     // httptest.NewServer(PlainHTTP) with a client from 127.0.0.1 and subnet 127.0.0.0/8 → GET /tftp/arm64/snp.efi returns 200 "arm64-efi" (the idle wrapper's SetWriteDeadline succeeds through Unwrap)
func TestBoot_WideEventStateAfterTransition(t *testing.T) // installing machine, WideEvents around TLS() → the logged line has state "install-unconfirmed"
func FuzzParseBootQuery(f *testing.F)                   // seeds: every query used above; input is a raw query string fed through url.ParseQuery (parse errors skip); property: never panics; on success MAC is "" or 17 chars, UUID is "" or 36 chars, Serial and Product are at most maxValueBytes
```

- [ ] **Step 2: Run to verify failure**

Run: `rtk proxy go test ./internal/boot/ -v` — Expected: FAIL, `undefined: NewHandlers`.

- [ ] **Step 3: Implement** `guard.go` and `handlers.go`.

- [ ] **Step 4: Run to verify pass**

Run: `rtk proxy go test ./internal/boot/... -race -v` — Expected: PASS.
Run: `rtk proxy go test ./internal/boot/ -run '^$' -fuzz=FuzzParseBootQuery -fuzztime=30s` — Expected: PASS.

- [ ] **Step 5: Gate and commit**

Run: `DOCKER_HOST=unix://$HOME/.colima/default/docker.sock rtk proxy task ci` — Expected: exit 0.
Run: `rtk proxy git add internal/boot` then `rtk proxy git commit -m "feat: boot routes /start, /boot and /tftp behind the source-subnet guard" -m "Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"`
Expected: one new commit; `rtk proxy sh -c "git cat-file commit HEAD | grep -c '^gpgsig'"` prints `1`.

---

### Task 14: /discover with rate limits
Kind: rewrite

**Files:**
- Create: `internal/boot/discover.go`, `internal/boot/ratelimit.go`, `internal/boot/discover_test.go`, `internal/boot/ratelimit_test.go`
- Modify: `internal/boot/handlers.go`

`handlers.go` registers `POST /discover` on `TLS()` and adds the limiter and the `hephaestus.discovery.reports` counter to `Handlers`.

**Interfaces:**
- Consumes: `fleet.Repository` (`Lookup`, `Observe`, `LatestReport`, `RecordReport`), Task 13's `Handlers`, options and guard.
- Produces (package `boot`):
  - Constants: `discoverPerSource = 8`, `discoverWindow = 10 * time.Minute`, `limiterCapacity = 4096`, `maxFormBytes = 16 << 10`, `maxNICs = 16` (it reuses Task 13's `maxValueBytes`), `retryAfterSeconds = "600"`.
  - `func newWindowLimiter(limit int, window time.Duration, capacity int, now func() time.Time) *windowLimiter`; `(*windowLimiter).Allow(key netip.Addr) bool` — a fixed window per key starting at its first hit; at `capacity` keys it first drops expired windows, and if still full refuses new keys; safe for concurrent use.
  - `func WithLimiter(limit int, window time.Duration, capacity int) Option` (tests only shrink it; defaults are the constants).
  - `type discoverInput struct { Identity fleet.Identity; Facts facts; MACs []string }`; `func parseDiscoverForm(form url.Values) (discoverInput, error)` — the validation rules below, before any I/O; `memsize` `""` or `"0"` is stored as `"unknown"` (EFI reports 0, see Known limitations).
  - `type nic struct { Index int "index"; MAC string "mac"; Chip string "chip" }` and `type facts struct` with JSON fields, in this order: `uuid`, `mac`, `serial`, `manufacturer`, `product`, `cpuvendor`, `cpumodel`, `memsize`, `platform`, `arch`, `nics` (all strings except `nics []nic`). Stored bytes are `json.Marshal(facts)`.
- `POST /discover` behaviour, in order: `http.MaxBytesReader(w, r.Body, maxFormBytes)` then `ParseForm` (a `*http.MaxBytesError` → 413, other errors → 400); any value longer than `maxValueBytes` → 400; `nic<N>_mac` for N in 0..15 (`nic<N>_chip` alongside) normalized with `NormalizeMAC` (an invalid one → 400); `mac` normalized (invalid → 400); no valid UUID and no MAC → 400. Identity = {NormalizeUUID(uuid), mac, serial, product}. Unknown machine: `limiter.Allow(source)` false → 429 + `Retry-After: 600`; `Observe` → `ErrFleetFull` → 503 + `Retry-After: 600`, `ErrUnidentifiable` → 400. Known machine: `Observe` → `ErrIdentityConflict` → 409 (the repository has marked the UUID junk and recorded the alert as row 2 does; when `Alerted` the `hephaestus.alerts{kind=uuid_conflict}` counter +1). Then: a machine created by this request (`Created`) is never charged a second token, so `RecordReport` follows directly; otherwise `LatestReport`: equal bytes → `RecordReport` (returns false) and 200 without charging; different → `limiter.Allow` (false → 429 + `Retry-After: 600`, the same header as the first 429 path) then `RecordReport`. `RecordReport` gets the normalized NIC MACs plus `mac`. Success → 200 `ok\n`. The counter `hephaestus.discovery.reports` gets `outcome` ∈ `stored`, `unchanged`, `rate_limited`, `fleet_full`, `conflict`, `invalid`; the wide event gets `outcome` and `machine_id`.

- [ ] **Step 1: Write the failing tests**

```go
func TestWindowLimiter(t *testing.T)   // fake clock: limit 8 → 8 Allow true then false; after window+1s true again; another key independent; capacity 2: third key false until the window passes, then true
func TestDiscover_StoresReport(t *testing.T) // form: uuid fixture, mac aa:bb:cc:00:00:01, serial S1, product P1, manufacturer ACME, nic0_mac aa:bb:cc:00:00:01, nic0_chip r8169, nic1_mac aa:bb:cc:00:00:02, nic1_chip igc, memsize 17179869184, arch x86_64, platform efi → 200 "ok\n"; machine discovered with both MACs; one report, source "ipxe", decoded facts: manufacturer "ACME", len(nics)==2, nics[1].chip "igc", memsize "17179869184"
func TestDiscover_UnchangedNotCharged(t *testing.T) // WithLimiter(8, 10m, 4096); same form 20 times → all 200; 1 report row
func TestDiscover_RateLimited(t *testing.T)          // 9 distinct unknown machines (UUIDs ending …001…009, MACs …01…09) from one source → 9th is 429 with Retry-After "600"
func TestDiscover_Conflict(t *testing.T)             // machine S1/P1 exists; post serial S2 → 409; no report; machine Junk; one alert.uuid_conflict audit row
func TestDiscover_NewMachineChargedOnce(t *testing.T) // WithLimiter(1, 10m, 4096): one unknown machine → 200 (one token covers creation and its first report); a second, changed report from the same source → 429 with Retry-After "600"
func TestDiscover_FirstContactAudited(t *testing.T)   // new machine → one audit row "transition.first_contact"
func TestDiscover_MemsizeZeroIsUnknown(t *testing.T)  // memsize "0" → stored facts.memsize "unknown"
func FuzzParseDiscoverForm(f *testing.F)              // seeds: every form above, as raw bodies fed through url.ParseQuery (parse errors skip); property: never panics; on success every string in Facts is at most maxValueBytes, len(Facts.NICs) <= maxNICs, every entry of MACs is 17 chars
func TestDiscover_RejectsOversizeAndMalformed(t *testing.T) // table → status: serial of 257 bytes → 400; 17 KiB body → 413; nic0_mac "zz" → 400; no uuid and no mac → 400; GET /discover → 405; 0 reports after all
func TestDiscover_HostileTextStoredVerbatim(t *testing.T)   // serial "<script>alert(1)</script>" → 200; decoded stored facts.serial equals it exactly
func TestDiscover_FleetFull(t *testing.T)            // 1024 machines bulk-inserted; new machine → 503, Retry-After "600"
func TestDiscover_OffSubnetRefused(t *testing.T)     // RemoteAddr "10.10.30.5:1" → 403
```

- [ ] **Step 2: Run to verify failure**

Run: `rtk proxy go test ./internal/boot/ -run 'TestWindowLimiter|TestDiscover|FuzzParseDiscoverForm' -v` — Expected: FAIL, `undefined: newWindowLimiter` (compile error).

- [ ] **Step 3: Implement** `ratelimit.go`, `discover.go`, and the route registration.

- [ ] **Step 4: Run to verify pass**

Run: `rtk proxy go test ./internal/boot/... -race -v` — Expected: PASS (Tasks 10, 13 and 14 tests).
Run: `rtk proxy go test ./internal/boot/ -run '^$' -fuzz=FuzzParseDiscoverForm -fuzztime=30s` — Expected: PASS.

- [ ] **Step 5: Gate and commit**

Run: `DOCKER_HOST=unix://$HOME/.colima/default/docker.sock rtk proxy task ci` — Expected: exit 0.
Run: `rtk proxy git add internal/boot` then `rtk proxy git commit -m "feat: /discover with per-source rate limits and a fleet cap" -m "Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"`
Expected: one new commit; `rtk proxy sh -c "git cat-file commit HEAD | grep -c '^gpgsig'"` prints `1`.

---

### Task 15: Let's Encrypt certificates (tlsacme)
Kind: rewrite

**Files:**
- Create: `internal/tlsacme/doc.go`, `manager.go`, `lego.go`, `repository.go`, `manager_test.go`, `lego_test.go`, `repository_test.go`, `repository_integration_test.go`, `main_integration_test.go`
- Create: `internal/tlsacme/tlsacmetest/doc.go`, `internal/tlsacme/tlsacmetest/selfsigned.go`
- Modify: `.taskfiles/go.yml`
- Modify: `go.mod`
- Modify: `go.sum`

`.taskfiles/go.yml`: `DB_PKGS` becomes `./internal/db/... ./internal/fleet/... ./internal/tlsacme/...`.

**Interfaces:**
- Consumes: `db.DB`, `datakey.Key`, `datakey.ErrDecrypt`, `secret.Secret`, `admin.Check`, `admin.Result`, `dbtest`.
- Produces (package `tlsacme`; all its tests are `package tlsacme_test` to avoid an import cycle with `tlsacmetest`):
  - `type Certificate struct { ChainPEM, KeyPEM []byte }`; `type Issuer interface { Obtain(ctx context.Context, domain string) (Certificate, error) }` (implementors: `*LegoIssuer`, `*tlsacmetest.SelfSigned`).
  - Repository: `func NewRepository(d *db.DB) *Repository`; `type Account struct { DirectoryURL string; KeySealed, Registration []byte; CreatedAt time.Time }`; `type StoredCertificate struct { Domain string; ChainSealed, KeySealed []byte; NotBefore, NotAfter, UpdatedAt time.Time }`; `SaveAccount(ctx, Account) error` (upsert); `LoadAccount(ctx, directoryURL string) (Account, bool, error)`; `SaveCertificate(ctx, StoredCertificate) error` (upsert by domain); `LoadCertificate(ctx, domain string) (StoredCertificate, bool, error)`. bun models map `acme_accounts` (`registration` TEXT ↔ `[]byte`) and `tls_certificates`.
  - `type LegoConfig struct { DirectoryURL string; CloudflareToken secret.Secret; Repo *Repository; Key *datakey.Key }`; `func NewLegoIssuer(cfg LegoConfig) *LegoIssuer`; `(*LegoIssuer).Load(ctx) error` — no account row for `DirectoryURL` → nil; a row whose key does not open → `fmt.Errorf("stored ACME account for %s cannot be decrypted with the configured data key: %w", directoryURL, err)` (wraps `datakey.ErrDecrypt`); `serve` calls it at startup, so a wrong data key stops startup for the account row exactly as for the certificate row; `(*LegoIssuer).Obtain` — returns `ctx.Err()` before any network call when ctx is done; account: `LoadAccount(DirectoryURL)`, else generate an ECDSA P-256 key, `client.Registration.Register(registration.RegisterOptions{TermsOfServiceAgreed: true})`, and only after `Register` succeeds save the PKCS#8 key sealed with AAD `acme_accounts/key/<directory url>` and the registration as JSON; client: `lego.NewConfig(user)`, `CADirURL = DirectoryURL`, `Certificate.KeyType = certcrypto.RSA2048`, `UserAgent = "hephaestus"`; DNS-01: `cloudflare.NewDefaultConfig()` with `AuthToken = CloudflareToken.Expose()`, `cloudflare.NewDNSProviderConfig`, `client.Challenge.SetDNS01Provider`; `client.Certificate.Obtain(certificate.ObtainRequest{Domains: []string{domain}, Bundle: true})` → `Certificate{ChainPEM: res.Certificate, KeyPEM: res.PrivateKey}`. Errors wrap as `acme: <step>: %w`.
  - `var ErrNoCertificate = errors.New("tlsacme: no certificate loaded yet")`; `func RenewAt(notBefore, notAfter time.Time) time.Time` = `notBefore + (notAfter−notBefore)*2/3`.
  - `type Option func(*Manager)`; `WithClock(func() time.Time)`, `WithErrorHandler(func(context.Context, error))`, `WithMeter(metric.Meter)` (default noop).
  - `func NewManager(repo *Repository, key *datakey.Key, iss Issuer, domain string, opts ...Option) (*Manager, error)` — registers `hephaestus.tls.certificate.days_left` (Float64ObservableGauge; observes only while a certificate is loaded) and `hephaestus.tls.renewal.failures` (Int64Counter).
  - `(*Manager).Load(ctx) error` — no stored row → nil, nothing loaded; a row that does not open → `fmt.Errorf("stored TLS certificate for %s cannot be decrypted with the configured data key: %w", domain, err)` (wraps `datakey.ErrDecrypt`); a leaf that does not cover `domain` → nil, nothing loaded.
  - `(*Manager).Reconcile(ctx) (time.Duration, error)` — loaded and `now < RenewAt` → `RenewAt−now`, nil. Otherwise `Obtain`, run in its own goroutine that sends to a buffered channel of size 1; when ctx is done first, `Reconcile` returns `0, ctx.Err()` and the abandoned result is discarded (lego v4.35.2 `Obtain` takes no context; the next start retries); on error: failures++, last error recorded, the error handler gets `obtain certificate for <domain>: <err>`, the counter +1, returns `min(10m·2^(failures−1), 6h)` and the error. On success: `tls.X509KeyPair` and leaf parse; a leaf whose `DNSNames` lacks `domain` → error `issued certificate does not cover <domain>` (counted as a failure); save sealed with AADs `tls_certificates/chain/<domain>` and `tls_certificates/key/<domain>`; load it; reset failures; return `RenewAt−now`.
  - `(*Manager).Run(ctx) error` — loops `Reconcile` with a `time.Timer` until ctx is done; returns nil then, without waiting for an abandoned `Obtain`.
  - `(*Manager).GetCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error)` — `ErrNoCertificate` until loaded. `GetCertificate`, `Status` and the checks are safe for concurrent use with `Run` (the current certificate sits behind an `atomic.Pointer` or a mutex).
  - `type Status struct { Loaded bool; NotAfter time.Time; Failures int; LastError string }`; `(*Manager).Status() Status`.
  - `(*Manager).Checks() []admin.Check` — both checks have `Interval: time.Second` (they read in-memory status, so readiness follows a fresh issue within a second) — `tls:certificate` (critical): pass when loaded (output `valid until <RFC3339>`), else fail `no certificate loaded`; `tls:expiry` (informational, observed days left float64, unit `d`): pass at ≥14 days, warn `certificate expires in <n> days` below (n = whole days, rounded down), fail `certificate expired` at ≤0, fail `no certificate loaded` when none.
- Produces (package `tlsacmetest`): `type SelfSigned struct { NotBefore, NotAfter time.Time; Domain string; Err error; Block <-chan struct{}; Calls atomic.Int64 }`; `(*SelfSigned).Obtain(ctx, domain) (tlsacme.Certificate, error)` — counts the call; when `Block` is non-nil it waits for it to close (ignoring ctx, like lego); returns `Err` when set; else a self-signed ECDSA P-256 certificate for `Domain` (or the requested domain when `Domain == ""`) with the given validity, PEM-encoded (`CERTIFICATE`, `PRIVATE KEY` PKCS#8).

- [ ] **Step 1: Add the dependency**

Run: `rtk proxy go get github.com/go-acme/lego/v4@v4.35.2` — Expected: exit 0; `go 1.26.0` unchanged.

- [ ] **Step 2: Write the failing tests.** Clock starts at `t0`; issuer `&tlsacmetest.SelfSigned{NotBefore: t0, NotAfter: t0.Add(90*24*time.Hour)}`; domain `hephaestus.example.test`; key = fixture; other key = `AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE=` (32 bytes of 0x01).

```go
func TestRenewAt(t *testing.T)                         // RenewAt(t0, t0+90d) == t0+60d
func TestReconcile_IssuesWhenEmpty(t *testing.T)       // wait == 60d, err nil; issuer.Calls.Load() == 1; Status().Loaded; GetCertificate → Leaf.DNSNames == ["hephaestus.example.test"]; stored ChainSealed does not contain "BEGIN CERTIFICATE"
func TestReconcile_NoReissueBeforeRenewAt(t *testing.T) // clock t0+59d → Calls.Load() still 1; wait == 24h
func TestReconcile_RenewsAtTwoThirds(t *testing.T)     // clock t0+60d → Calls.Load() == 2
func TestLoad_ReusesStoredCertificate(t *testing.T)    // second Manager on the same repo and key: Load → Loaded; Reconcile at t0+24h → its issuer Calls.Load() == 0
func TestLoad_WrongDataKeyFailsLoudly(t *testing.T)    // Manager with the other key: Load → errors.Is(err, datakey.ErrDecrypt) and message contains "cannot be decrypted with the configured data key"
func TestLoad_OtherDomainIgnored(t *testing.T)         // stored for other.example.test; Manager for hephaestus.example.test: Load → nil, !Loaded
func TestReconcile_FailureBacksOff(t *testing.T)       // issuer Err set: seven Reconciles → waits [10m 20m 40m 80m 160m 320m 360m]; handler got 7 errors containing "obtain certificate for hephaestus.example.test"; Status().Failures == 7
func TestReconcile_RejectsCertForWrongName(t *testing.T) // issuer Domain "other.example.test" → error contains "does not cover hephaestus.example.test"; !Loaded
func TestGetCertificate_NoneYet(t *testing.T)          // errors.Is(err, ErrNoCertificate)
func TestChecks(t *testing.T)                          // not loaded: tls:certificate fail "no certificate loaded"; loaded, clock t0+60d (30 days left): both pass, expiry observed 30.0 unit "d"; clock t0+80d: expiry warn "certificate expires in 10 days"; clock t0+91d: expiry fail "certificate expired"
func TestDaysLeftMetric(t *testing.T)                  // WithMeter(telemetry metrics); after issuing → metrics body contains "hephaestus_tls_certificate_days_left"
func TestRun_StopsOnCancel(t *testing.T)               // goleak; Run in a goroutine; cancel after the first issue (poll Status().Loaded); Run returns nil
func TestRun_AbandonsBlockedObtain(t *testing.T)       // issuer Block = an open channel; Run; cancel after 50ms → Run returns nil within 1s; then close(Block); goleak.VerifyNone (the abandoned goroutine has exited)
func TestLegoIssuer_LoadWrongKeyFails(t *testing.T)    // SaveAccount sealed under the fixture key (AAD "acme_accounts/key/<dir>"); NewLegoIssuer with the other key: Load → errors.Is(err, datakey.ErrDecrypt), message contains "stored ACME account for" and "cannot be decrypted with the configured data key"; empty repo → Load nil
func TestLegoIssuer_CanceledContext(t *testing.T)      // canceled ctx → errors.Is(err, context.Canceled); no account row
func TestLegoIssuer_BadDirectory(t *testing.T)         // DirectoryURL = httptest server answering 404 → error with prefix "acme: "; no account row
func runRepositorySuite(t *testing.T, open func(testing.TB) *db.DB) // subtests: "AccountRoundTrip" (save, load equal, missing → false); "CertificateUpsert" (save twice for one domain → load returns the second; NotAfter Equal); "CertificateMissing" (false)
func TestRepository_SQLite(t *testing.T)   // runRepositorySuite(t, dbtest.SQLite)
func TestRepository_Postgres(t *testing.T) // integration file; TestMain in main_integration_test.go
```

- [ ] **Step 3: Run to verify failure**

Run: `rtk proxy go test ./internal/tlsacme/... -v` — Expected: FAIL with `[build failed]` or `[setup failed]` (`no non-test Go files` / `no required module provides package go.jacaudi.dev/hephaestus/internal/tlsacme…`).

- [ ] **Step 4: Implement** the files above; extend `DB_PKGS`.

- [ ] **Step 5: Run to verify pass on both databases**

Run: `rtk proxy go test ./internal/tlsacme/... -race -v` — Expected: PASS.
Run: `DOCKER_HOST=unix://$HOME/.colima/default/docker.sock rtk proxy go test -tags integration ./internal/tlsacme/... -race -run Postgres -v` — Expected: PASS, 3 subtests.

- [ ] **Step 6: Gate and commit**

Run: `DOCKER_HOST=unix://$HOME/.colima/default/docker.sock rtk proxy task ci` — Expected: exit 0; `cover:db` prints nothing.
Run: `rtk proxy git add go.mod go.sum .taskfiles/go.yml internal/tlsacme` then `rtk proxy git commit -m "feat: Let's Encrypt certificates via DNS-01 on Cloudflare, sealed under the data key" -m "Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"`
Expected: one new commit; `rtk proxy sh -c "git cat-file commit HEAD | grep -c '^gpgsig'"` prints `1`.

---

### Task 16: `hephaestus serve`
Kind: rewrite

**Files:**
- Create: `cmd/hephaestus/serve.go`, `cmd/hephaestus/serve_test.go`
- Modify: `cmd/hephaestus/main.go`

`main.go` adds `newServeCmd()` to the root command.

**Interfaces:**
- Consumes: `loadConfig`/`serveConfig` (Task 6); `telemetry.NewLogger`, `NewMetrics`, `WideEvents`; `admin.NewRegistry`, `Lifecycle`, `NewHandler`, `DiskCheck`, `Info`; `db.Open`, `Migrate`, `Checks`; `fleet.NewRepository`; `tlsacme.NewRepository`, `NewManager`, `NewLegoIssuer`, `Issuer`; `boot.NewHandlers`, `WithMeter`; `ipxe.Files`; vendored `dhcp.NewProxy`, `tftp.NewServer`; `tlsacmetest.SelfSigned` (tests).
- Produces (package `main`):
  - `func newServeCmd() *cobra.Command` — `Use: "serve"`, Short `Run the boot plane and the admin listener`, `Long` and `Example` naming the required env vars. RunE: `loadConfig` → `telemetry.NewLogger(cmd.OutOrStdout(), cfg.LogFormat, cfg.LogLevel, buildVersion())` → `legolog.Logger = slog.NewLogLogger(logger.Handler(), slog.LevelInfo)` (package `github.com/go-acme/lego/v4/log`) → listeners via `net.ListenConfig{}` `.Listen(ctx, "tcp", …)` on `<provisioning address>:80`, `…:443` and `cfg.AdminAddr.String()` → `ipxe.Files()` → `runServe(cmd.Context(), cfg, logger, serveDeps{newIssuer: legoIssuerFactory(cfg), httpLn, httpsLn, adminLn, udp: true, files})`.
  - `func legoIssuerFactory(cfg serveConfig) func(ctx context.Context, repo *tlsacme.Repository, key *datakey.Key) (tlsacme.Issuer, error)` — the returned func builds `tlsacme.NewLegoIssuer(LegoConfig{DirectoryURL: cfg.ACMEDirectoryURL, CloudflareToken: cfg.CloudflareToken, Repo: repo, Key: key})`, calls its `Load(ctx)` (an error is returned), and returns it; mark it `//nolint:ireturn // factory pattern (go-standards §12)` wherever the linter reports it.
  - `type serveDeps struct { newIssuer func(context.Context, *tlsacme.Repository, *datakey.Key) (tlsacme.Issuer, error); httpLn, httpsLn, adminLn net.Listener; udp bool; files map[string][]byte }` (`udp` false only in tests: it skips proxyDHCP and TFTP, which bind fixed privileged UDP ports).
  - `func runServe(ctx context.Context, cfg serveConfig, logger *slog.Logger, deps serveDeps) error`, in order: `os.MkdirAll(cfg.DataDir, 0o700)`; metrics (deferred `Shutdown`); `db.Open` + `Migrate` (deferred `Close`); repositories; `iss, err := deps.newIssuer(ctx, acmeRepo, cfg.DataKey)` (an error returns immediately); `tlsacme.NewManager(acmeRepo, cfg.DataKey, iss, cfg.FQDN, WithMeter, WithErrorHandler(logs at Warn "certificate renewal failed"))` and `Load` (an error returns immediately); registry with `db.Checks()`, `admin.DiskCheck("disk:data_dir", cfg.DataDir, 0.10)`, `mgr.Checks()`, then `RunOnce`; `boot.NewHandlers(repo, cfg.FQDN, cfg.Subnet, deps.files, boot.WithMeter(metrics.Meter()))`; one startup Info log with `fqdn`, `address`, `subnet`, `admin_address`, `data_dir`, `database` (the dialect only).
  - Servers (all with `ReadHeaderTimeout 5s`, `ReadTimeout 10s`, `IdleTimeout 60s`, `MaxHeaderBytes 1<<20`): admin (`WriteTimeout 10s`, handler `telemetry.WideEvents(admin.NewHandler(registry, life, metrics.Handler(), admin.Info{Version: buildVersion(), ReleaseID: commit, Started: time.Now()}), logger, slog.LevelInfo)`); boot HTTP (`WriteTimeout 60s`, handler `telemetry.WideEvents(metrics.Instrument(h.PlainHTTP(), "boot-http"), logger, slog.LevelInfo)`, `Serve(deps.httpLn)` — plaintext by design §5.10, comment says so); boot HTTPS (`WriteTimeout 60s`, `TLSConfig{MinVersion: tls.VersionTLS13, GetCertificate: mgr.GetCertificate}`, handler as above with `h.TLS()` and operation `boot-https`, `ServeTLS(deps.httpsLn, "", "")`).
  - Lifecycle: start every server, `registry.Run`, `mgr.Run`, and (when `udp`) `dhcp.NewProxy(cfg.Address.String(), 80, cfg.Interface, false, logger.With("component", "dhcp_proxy")).Run` and `tftp.NewServer(cfg.Address.String(), deps.files, logger.With("component", "tftp")).Run`; then `life.Set(admin.Ready)`. A component that fails stops all of them and `runServe` returns its error. On ctx done: `life.Set(Draining)`; cancel the proxy's context and wait for it to return; `Shutdown` the HTTP and HTTPS servers with a `context.WithTimeout(context.WithoutCancel(ctx), cfg.ShutdownGrace)`; cancel TFTP, `registry.Run`, `mgr.Run` and wait (`mgr.Run` returns without waiting for an abandoned ACME `Obtain`); shut the admin server down with its own `context.WithTimeout(context.WithoutCancel(ctx), cfg.ShutdownGrace)`; `life.Set(Stopped)`; return `errors.Join` of the shutdown errors (nil on a clean stop).

- [ ] **Step 1: Write the failing tests.** `testServeConfig(t)` builds a `serveConfig` directly: FQDN `hephaestus.example.test`, Interface `lo0`, Address `127.0.0.1`, Subnet `127.0.0.0/8`, DataDir `t.TempDir()`, DatabaseURL `sqlite:<dir>/h.db`, fixture DataKey, ShutdownGrace 5s, LogFormat json, LogLevel info. Listeners `(&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")`. `newIssuer` returns `&tlsacmetest.SelfSigned{NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(90*24*time.Hour)}`. `udp: false`; files `{"amd64/snp.efi": []byte("amd64-efi"), "arm64/snp.efi": []byte("arm64-efi")}`.

```go
func TestRunServe_ServesAndDrains(t *testing.T)
// defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
// run in a goroutine; poll GET http://<admin>/readyz every 100ms until 200 (fail after 10s)
// GET http://<http>/start → 200, body contains "chain https://hephaestus.example.test/boot?"
// GET https://<https>/boot?uuid=4c4c4544-0042-3510-8052-b4c04f4e4d32&mac=aa-bb-cc-00-00-01&arch=x86_64&serial=S1&product=P1
//   (client tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS13}) → 200, body HasPrefix "#!ipxe\nparams\n"; resp.TLS.Version == tls.VersionTLS13
// GET http://<admin>/metrics → contains "hephaestus_boot_decisions_total"
// cancel ctx → runServe returns nil within 10s
func TestRunServe_TLS12Refused(t *testing.T)      // client MaxVersion tls.VersionTLS12 → handshake error
func TestRunServe_WrongDataKeyFails(t *testing.T) // first run (fixture key) until ready, then stop; second run on the same DataDir and DatabaseURL with the other key `AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE=` → returns an error containing "cannot be decrypted with the configured data key" without becoming ready
func TestServeCmd_InvalidConfig(t *testing.T)     // clearEnv(t); root args ["serve"] → exitCode(err) == 1 and err.Error() contains "HEPHAESTUS_FQDN is required"
func TestRunServe_IssuerFactoryErrorStops(t *testing.T) // newIssuer returns errors.New("account check failed") → runServe returns an error containing it; /readyz never answered
func TestRunServe_ShutdownAbandonsObtain(t *testing.T)  // ShutdownGrace 2s; newIssuer returns SelfSigned with Block = an open channel (never issues, so never ready); wait until GET /healthz answers 200; cancel ctx → runServe returns within 3s (ShutdownGrace + 1s); then close(Block)
func TestRunServe_AdminWideEvents(t *testing.T)   // logger writing to a buffer; GET /healthz on the admin listener → the buffer holds a "request" line with path "/healthz"
```

- [ ] **Step 2: Run to verify failure**

Run: `rtk proxy go test ./cmd/hephaestus/ -run 'TestRunServe|TestServeCmd' -v` — Expected: FAIL, `undefined: runServe`.

- [ ] **Step 3: Implement** `serve.go` and register the command in `main.go`.

- [ ] **Step 4: Run to verify pass**

Run: `rtk proxy go test ./cmd/hephaestus/ -race -v` — Expected: PASS (all cmd tests).
Run: `rtk proxy task go:cross` — Expected: exit 0.

- [ ] **Step 5: Gate and commit**

Run: `DOCKER_HOST=unix://$HOME/.colima/default/docker.sock rtk proxy task ci` — Expected: exit 0.
Run: `rtk proxy git add cmd/hephaestus` then `rtk proxy git commit -m "feat: hephaestus serve wires the boot plane, TLS and the admin listener" -m "Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"`
Expected: one new commit; `rtk proxy sh -c "git cat-file commit HEAD | grep -c '^gpgsig'"` prints `1`.

---

### Task 17: Container image and README
Kind: rewrite

**Files:**
- Create: `Dockerfile`, `.dockerignore`, `README.md`
- Modify: `taskfile.yml`

`taskfile.yml` gains `image:build`.

**Interfaces:**
- Consumes: the `serve` and `version` commands.
- Produces: image `hephaestus:dev` (linux/arm64 locally; the Dockerfile builds both linux/amd64 and linux/arm64 through `--platform=$BUILDPLATFORM` cross-compilation).

`Dockerfile`:
```dockerfile
# syntax=docker/dockerfile:1
FROM --platform=$BUILDPLATFORM golang:1.27.4-trixie@sha256:b54b2639de8c1bfe538ec5ffc45f29b1932235ac5f6d63c059853646677112d9 AS build
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
ARG COMMIT=unknown
ARG DATE=unknown
ENV DEBIAN_SNAPSHOT=20261104T212339Z
RUN set -eux; \
    rm -f /etc/apt/sources.list.d/debian.sources; \
    printf 'Types: deb\nURIs: http://snapshot.debian.org/archive/debian/%s\nSuites: trixie trixie-updates\nComponents: main\nSigned-By: /usr/share/keyrings/debian-archive-keyring.pgp\n\nTypes: deb\nURIs: http://snapshot.debian.org/archive/debian-security/%s\nSuites: trixie-security\nComponents: main\nSigned-By: /usr/share/keyrings/debian-archive-keyring.pgp\n' "$DEBIAN_SNAPSHOT" "$DEBIAN_SNAPSHOT" > /etc/apt/sources.list.d/snapshot.sources; \
    apt-get -o Acquire::Check-Valid-Until=false update; \
    apt-get install -y --no-install-recommends libcap2-bin=1:2.75-10+deb13u1+b3; \
    rm -rf /var/lib/apt/lists/*
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags "-s -w -X main.version=${VERSION} -X main.commit=${COMMIT} -X main.date=${DATE}" -o /out/hephaestus ./cmd/hephaestus \
    && setcap cap_net_bind_service=+ep /out/hephaestus \
    && getcap /out/hephaestus

FROM gcr.io/distroless/static-debian13:nonroot@sha256:aafe993665bf6200db38a02c11a9351e782a84b6a45e7f33b6695638c88e3931
ARG VERSION=dev
ARG COMMIT=unknown
ARG DATE=unknown
LABEL org.opencontainers.image.title="hephaestus" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.revision="${COMMIT}" \
      org.opencontainers.image.created="${DATE}"
COPY --from=build /out/hephaestus /hephaestus
USER 65532:65532
ENTRYPOINT ["/hephaestus"]
CMD ["serve"]
```
`.dockerignore`, exactly:
```
.git
bin
build/ipxe/.work
build/ipxe/out
docs
```
`taskfile.yml` addition:
```yaml
  image:build:
    desc: Build and load hephaestus:dev for linux/arm64 (needs DOCKER_HOST)
    vars:
      VERSION: {sh: git describe --tags --always --dirty}
      COMMIT: {sh: git rev-parse --short HEAD}
      DATE: {sh: date -u +%Y-%m-%dT%H:%M:%SZ}
    cmds:
      - docker buildx build --platform linux/arm64 --load --progress=plain --build-arg VERSION={{.VERSION}} --build-arg COMMIT={{.COMMIT}} --build-arg DATE={{.DATE}} -t hephaestus:dev .
```
`README.md` (under 120 lines), in this order: one-sentence hook (design §1's job statement); Quick Start (`task build`, then the `HEPHAESTUS_*` env table from Task 6 with each default and an `openssl rand -base64 32` data-key example, then `./bin/hephaestus serve`); Prerequisites (Go 1.26+, Task, Docker via colima for Postgres tests and iPXE builds, hadolint); Architecture (a short ASCII sketch: proxyDHCP/TFTP/HTTP:80/HTTPS:443 on the VLAN IP, admin :9090 on the management address, SQLite or Postgres, Let's Encrypt via Cloudflare DNS-01); Build (`task ci`, `task ipxe:build`, `task image:build`); Licences (design §10: GPLv2 iPXE binaries built from commit `9261ebe` with `build/ipxe/`; MPL-2.0 vendored files under `internal/third_party/omniprovider/`); M1 limits (no UI, no installs: assigned machines hold at row 8).

- [ ] **Step 1: Lint the Dockerfile**

Run: `rtk proxy sh -c "hadolint Dockerfile && echo hadolint-ok"` — Expected: `hadolint-ok`.

- [ ] **Step 2: Build the image**

Run: `DOCKER_HOST=unix://$HOME/.colima/default/docker.sock rtk proxy task image:build`
Expected: exit 0; the build log contains `/out/hephaestus cap_net_bind_service=ep`.

- [ ] **Step 3: Verify the image**

Run: `DOCKER_HOST=unix://$HOME/.colima/default/docker.sock docker run --rm hephaestus:dev version --json`
Expected: JSON whose `commit` equals `git rev-parse --short HEAD`.
Run: `DOCKER_HOST=unix://$HOME/.colima/default/docker.sock docker image inspect hephaestus:dev --format '{{.Config.User}}'`
Expected: `65532:65532`.
Run: `DOCKER_HOST=unix://$HOME/.colima/default/docker.sock docker run --rm hephaestus:dev serve; echo "exit=$?"`
Expected: stderr contains `HEPHAESTUS_FQDN is required`; `exit=1`.

- [ ] **Step 4: Gate and commit**

Run: `DOCKER_HOST=unix://$HOME/.colima/default/docker.sock rtk proxy task ci` — Expected: exit 0; hadolint lints both Dockerfiles.
Run: `rtk proxy git add Dockerfile .dockerignore README.md taskfile.yml` then `rtk proxy git commit -m "feat: distroless nonroot image with file capabilities, and the README" -m "Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"`
Expected: one new commit; `rtk proxy sh -c "git cat-file commit HEAD | grep -c '^gpgsig'"` prints `1`.
