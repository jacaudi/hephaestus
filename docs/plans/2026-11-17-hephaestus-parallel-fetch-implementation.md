# Parallel utility downloads and clean fetch errors — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Several utility downloads run at once (default 4, `HEPHAESTUS_FETCH_WORKERS`). A trickling upstream is cut off. Fetch errors carry only printable text.

**Architecture:**
- `toolcache.Fetcher.Run` starts N worker loops over the existing claim, download and record cycle.
- `claimFetch` moves on to the next candidate when it loses a race.
- Each worker keeps its own held outcome and resets only its own row.
- Free-space checks subtract other downloads' reservations.
- A rate check fails transfers under 32 KiB/s over 2 minutes.
- `fleet.PrintableText` cleans every fetch error once.

**Tech Stack:** Go 1.26 (toolchain go1.27.4), bun on SQLite and Postgres, oras-go v3, viper config, `task ci`.

**Spec:** `docs/designs/2026-11-17-hephaestus-parallel-fetch-design.md` (approved 2026-11-17).

**Owner directives:**
- Commits are UNSIGNED until the merge (`git -c commit.gpgsign=false commit`). The merge re-signs every commit with `git rebase --gpg-sign --force-rebase <merge-base>`. Never `--no-gpg-sign`.
- `HEPHAESTUS_FETCH_WORKERS` defaults to 4.

## Global Constraints

- TLS 1.3 minimum for every outbound connection; never propose a 1.2 floor.
- `HEPHAESTUS_FETCH_WORKERS`: an integer from 1 to 16, default 4. An invalid value stops serve at startup.
- Throughput floor: 32 KiB/s (`32 << 10` bytes per second), averaged over 2 minutes. Not configurable.
- Each download reserves 2 × object size + 1 GiB (`spaceReserve`) while in flight. A new download needs `free − reserved by others ≥ its own need`.
- Startup reconciliation keeps `ResetFetching` (all rows). The runtime held-outcome reset is per row.
- `fleet.CleanText` (M1 serial/product cleaning) is unchanged.
- Lock order in every transaction touching both tables: `machines`, then `utility_cache`.
- Repository behaviour is tested on SQLite and Postgres through the existing suites (`runUtilitySuite` plus the `_integration_test.go` Postgres runs).
- Authorized `//nolint` only: G505/G401 (sha1) and G204 (gpg exec) in `cmd/catalogpin`, and the errcheck form on a discarded production `errors.AsType`. No others.
- Gate per task: `go test ./... -race` and `DOCKER_HOST=unix://$HOME/.colima/default/docker.sock rtk proxy task ci`. The ci gate must exit 0 with `0 issues.`, `No vulnerabilities found.`, and no `not executed:` result line from cover:db.
- Commit trailer: `Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>`. No `claude.ai/code/session_` URL anywhere.
- Never prune shared Docker state. Touch no remote.

## Review Focus

1. **Shutdown mid-download with N workers.** `Run` returns nil only after every worker loop returned, and no goroutine leaks. Covered in Task 3: `TestFetcher_PoolShutdownLeavesNoGoroutines`.
2. **Two entries whose files share a digest, fetched at the same time.** Both reach `ready`; concurrent `PushBlob` of one digest must not fail either. Covered in Task 3: `TestFetcher_PoolSharedBlob`.
3. **A reservation is released on every exit: failure, integrity break, cancel.** A later download is not refused for space that is no longer used. Covered in Task 3: `TestFetcher_ReservationReleasedOnFailure`.
4. **A resumed (206) transfer.** The floor measures this transfer's own bytes from its own start, so a fast resume passes. Covered in Task 4: `TestFetcher_FloorMeasuresThisTransfer`.
5. **`Workers` = 1 behaves exactly as before.** The whole existing fetcher test suite runs with `Workers: 1` unchanged. Covered in Task 3, Step 4.

---

### Task 1: Printable fetch errors (security record S1)

**Files:**
- Modify: `internal/fleet/identity.go` (next to `CleanText`)
- Modify: `internal/fleet/utility_repository.go` (`storedCause`)
- Modify: `internal/toolcache/fetcher.go` (the failure branch of the claim/record cycle, today in `runOnce`)
- Modify: `cmd/hephaestus/serve.go` (`logFetchEvent`)
- Test: `internal/fleet/identity_test.go`, `internal/fleet/utility_repository_test.go`, `internal/toolcache/fetcher_test.go`, `cmd/hephaestus/cli_test.go`

**Interfaces:**
- Produces: `func PrintableText(s string) string` in package `fleet`. It returns s with invalid UTF-8 removed and every rune for which `unicode.IsGraphic` is false removed. The ASCII space is kept; tab, CR and LF are dropped.

- [ ] **Step 1: Write the failing tests**

```go
func TestPrintableText(t *testing.T) // table:
// "plain text" → "plain text"
// "a\x00b" → "ab"; "a\tb\r\n" → "ab"; "a\x1b[2Jb" → "a[2Jb"
// "a\u009bb" → "ab"; "a‮b" → "ab"; "a​b" → "ab"; "a b" → "ab"
// "café 世" → "café 世"; "bad\xffutf8" → "badutf8"
```

Write the `\u` escapes through Bash with the backslash built from a variable, as the repository's earlier tasks did, and check the bytes with `grep -c 'u009b'`.

Add a `runUtilitySuite` subtest `FailureCauseIsPrintable`. Record a failure whose `Cause` is `"x\u009by‮z​"`, then assert the stored `LastError == "xyz"`.

Add `TestFetcher_FailureCauseIsPrintable`. Make `job.run` fail with an error whose text holds U+009B, through a test transport (an `http.RoundTripper` on `FetcherConfig.Client` that returns that error). The status line is already `%q`-escaped, so it is not a usable route. Assert the row's `LastError` and the emitted `FetchEvent`'s error text hold no rune with `!unicode.IsGraphic(r) && r != ' '`.

Add `TestUtilityList_JSONCauseIsPrintable`: a broken row stored through the repository with a C1 cause; `utility list --json` output holds no byte sequence of U+009B.

- [ ] **Step 2: Run to verify they fail**

Run: `go test ./internal/fleet/ ./internal/toolcache/ ./cmd/hephaestus/ -run 'PrintableText|FailureCauseIsPrintable|JSONCauseIsPrintable' -race`
Expected: FAIL. `TestPrintableText` fails to compile (`undefined: PrintableText`); the others fail with the control runes present.

- [ ] **Step 3: Implement**

- `PrintableText`, per the Interfaces line above.
- `storedCause` applies `PrintableText` before the `MaxCauseBytes` cap.
- The fetcher builds the cause once, `fleet.PrintableText(err.Error())`, and uses it for `FetchFailure.Cause`.
- `logFetchEvent` logs `fleet.PrintableText(ev.Err.Error())` as `error`.
- `CleanText` is unchanged.

- [ ] **Step 4: Run to verify they pass**

Run: the Step 2 command, then the gate.
Expected: PASS; gate exit 0.

- [ ] **Step 5: Commit**

```bash
git add internal/fleet/identity.go internal/fleet/identity_test.go internal/fleet/utility_repository.go internal/fleet/utility_repository_test.go internal/toolcache/fetcher.go internal/toolcache/fetcher_test.go cmd/hephaestus/serve.go cmd/hephaestus/cli_test.go
git -c commit.gpgsign=false commit -m "fix: address security audit finding S1" -m "Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>"
```

No comment, test name or doc may mention an audit, finding or attack.

### Task 2: Claim and reset safe for several workers

**Files:**
- Modify: `internal/fleet/utility_repository.go` (`claimFetch`, new `ResetFetchingRow`)
- Test: `internal/fleet/utility_repository_test.go`, `internal/fleet/utility_repository_integration_test.go`

**Interfaces:**
- Produces: `func (r *Repository) ResetFetchingRow(ctx context.Context, key CacheKey, pin string, at time.Time) (bool, error)`. It moves the row from `fetching` to `wanted` only while it is `fetching` at `pin`, and returns whether it moved.
- Changes: `claimFetch` orders the claimable rows as today. When the conditional update of the first candidate moves nothing, it tries the next, until one moves or none are left. `(*Repository).ClaimFetch` keeps its signature.

- [ ] **Step 1: Write the failing tests**

Add these `runUtilitySuite` subtests, and the same names in the Postgres integration file using two `Repository` values over one database:

- `ClaimSkipsRowTakenMeanwhile`. Two `wanted` rows k1 and k2. Move k1 to `fetching` with a raw update, then call `ClaimFetch` and assert it returns k2.
  - If the current code reads its candidates first, add a test hook or ordering; the reviewer accepts a `claimFetch` unit test that injects the lost race between the select and the update.
- `ConcurrentClaimsGetDifferentRows` (Postgres file). With two rows, two goroutines claiming at once get two different keys. With one row, exactly one gets `ok == true`.
- `ResetFetchingRowOnlyItsOwn`. Rows k1 and k2 are both `fetching`. `ResetFetchingRow(k1, pin)` returns true. Afterwards k1 is `wanted` and k2 is still `fetching`. A second call returns false. A call with the wrong pin returns false.

- [ ] **Step 2: Run to verify they fail**

Run: `go test ./internal/fleet/ -run 'ClaimSkips|ConcurrentClaims|ResetFetchingRow' -race`, plus the Postgres run the same way `task ci` invokes the integration tag.
Expected: FAIL. `ResetFetchingRow` is undefined, and the claim returns `ok == false` after a lost race.

- [ ] **Step 3: Implement** `ResetFetchingRow` and the candidate loop in `claimFetch`.

- [ ] **Step 4: Run to verify they pass**

Run: the Step 2 commands, then the gate.
Expected: PASS; gate exit 0.

- [ ] **Step 5: Commit**

```bash
git -c commit.gpgsign=false commit -m "feat: a lost claim tries the next row; reset one fetching row" -m "Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>"
```

Stage the files explicitly.

### Task 3: Worker pool and free-space reservations

**Files:**
- Modify: `internal/toolcache/fetcher.go`
- Test: `internal/toolcache/fetcher_test.go`

**Interfaces:**
- Consumes: Task 2's `ResetFetchingRow`, and `ClaimFetch` being safe to call concurrently.
- Produces:
  - `const DefaultFetchWorkers = 4` and `const MaxFetchWorkers = 16` in package `toolcache`.
  - `FetcherConfig` gains `Workers int`. 0 means `DefaultFetchWorkers`. Any value below 0 or above `MaxFetchWorkers` makes `NewFetcher` return an error naming the range.
  - `Fetcher.Run(ctx) error` starts `Workers` loops and returns nil only after every loop has returned (gate-1 B5).
- Behaviour:
  - The held outcome is per worker loop. Its reset calls `ResetFetchingRow(key, pin, at)` instead of `ResetFetching`, and the reset event is emitted only when it moved the row.
  - A loop that claimed a row polls again at once. A loop that claimed nothing waits `poll`.
  - Free space: the fetcher keeps a mutex-guarded sum of reservations. The current per-object check becomes "reserve `2*size + spaceReserve` if `free − reserved ≥ need`". The reservation is held until the job's `close`, which releases all of it whatever the outcome. The refusal message names free, reserved and need.

- [ ] **Step 1: Write the failing tests**

- `TestFetcher_PoolSlowDoesNotBlockFast`, the stall the owner described. `Workers: 2`; entry A's upstream handler blocks until the test releases it, and entry B's upstream is normal. Both rows are `wanted`. Assert B reaches `ready` while A is still `fetching`, then release A.
- `TestFetcher_PoolShutdownLeavesNoGoroutines`. `Workers: 4`, two blocked downloads. Cancel the context. `Run` returns nil, and `goleak.VerifyNone` passes with the existing ignores.
- `TestFetcher_PoolSharedBlob`. Two catalog entries whose files have identical content are fetched with `Workers: 2`. Both reach `ready`.
- `TestFetcher_ReservationBlocksSecond`. The `statfs` hook reports free space for exactly one large object's need. With `Workers: 2`, the second download fails the space check (default class, with a message naming reserved) while the first is blocked. After the first finishes, a retry succeeds.
- `TestFetcher_ReservationReleasedOnFailure`. The first download fails with an integrity error. The second, with the same `statfs`, then passes the space check.
- `TestFetcher_HeldResetOnlyOwnRow`. Worker A's record fails twice (the existing failing-repository hook) while worker B holds another row `fetching`. Afterwards A's row is `wanted` and B's is still `fetching`.
- `TestNewFetcher_Workers`. 0 gives 4, 16 is accepted, and 17 and -1 are errors.

- [ ] **Step 2: Run to verify they fail**

Run: `go test ./internal/toolcache/ -run 'Pool|Reservation|HeldResetOnlyOwnRow|NewFetcher_Workers' -race`
Expected: FAIL. It fails to compile (`unknown field Workers`), then the stall and reservation assertions fail.

- [ ] **Step 3: Implement** the pool, the per-worker held state, the per-row reset and the reservations, as the Interfaces block says. Choose `sync.WaitGroup` or `errgroup` (the design leaves this to you), and keep `Run` returning nil on cancel.

- [ ] **Step 4: Run the whole toolcache suite**

Run: `go test ./internal/toolcache/ -race -count=3`, then the gate.
Expected: PASS three times. Every pre-existing fetcher test passes unchanged; set `Workers: 1` only in the tests whose ordering assertions need a single worker, and list them in the report. Gate exit 0.

- [ ] **Step 5: Commit**

```bash
git -c commit.gpgsign=false commit -m "feat: several fetch workers with shared free-space reservations" -m "Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>"
```

### Task 4: Throughput floor

**Files:**
- Modify: `internal/toolcache/fetcher.go` (next to `idleReader`)
- Test: `internal/toolcache/fetcher_test.go`

**Interfaces:**
- Produces:
  - `const MinThroughput = 32 << 10` (bytes per second) and `const ThroughputWindow = 2 * time.Minute` in package `toolcache`.
  - A reader wrapping the response body, using the fetcher's clock (`WithClock`). Once the transfer has run at least `ThroughputWindow`, a `Read` fails with `errTooSlow` when the bytes received in the last `ThroughputWindow` are fewer than `MinThroughput × 120`.
  - `errTooSlow` is in the default (retryable) class. Its message is `the upstream sent fewer than 32 KiB/s over 2m0s`.
  - The 60 s idle reader stays.

- [ ] **Step 1: Write the failing tests** (on an injected clock; no real sleeping beyond what the existing tests do)

- `TestFetcher_FloorCutsTrickle`. The fake upstream writes 1 KiB per write. The clock advances 1 s per body `Read`, through a hook. The object is large enough to outlast 120 s. Assert: the row is `failed`, `attempts` is 1, the event class is the default class, and `LastError` contains `32 KiB/s`.
- `TestFetcher_FloorAllowsSteady`. The same setup at 64 KiB per write per simulated second. The row reaches `ready`.
- `TestFetcher_FloorMeasuresThisTransfer`. A spool already holds half the object, so the transfer resumes with a 206 and sends at 64 KiB per simulated second. It reaches `ready`; bytes already on disk are never counted.

- [ ] **Step 2: Run to verify they fail**

Run: `go test ./internal/toolcache/ -run 'Floor' -race`
Expected: FAIL. The trickle reaches `ready` (or times out the test) instead of failing.

- [ ] **Step 3: Implement** the rate reader per the Interfaces block. A ring of (time, cumulative bytes) samples is enough; the design leaves the type name to you.

- [ ] **Step 4: Run to verify they pass**

Run: `go test ./internal/toolcache/ -race -count=3`, then the gate.
Expected: PASS; gate exit 0.

- [ ] **Step 5: Commit**

```bash
git -c commit.gpgsign=false commit -m "feat: cut off an upstream slower than 32 KiB/s over 2 minutes" -m "Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>"
```

### Task 5: `HEPHAESTUS_FETCH_WORKERS`, serve wiring and docs

**Files:**
- Modify: `cmd/hephaestus/config.go` (`rawConfig` gains `Fetch struct { Workers int \`mapstructure:"workers"\` } \`mapstructure:"fetch"\``; `serveConfig` gains `FetchWorkers int`; `v.SetDefault("fetch.workers", toolcache.DefaultFetchWorkers)`; validation)
- Modify: `cmd/hephaestus/serve.go` (pass `Workers: cfg.FetchWorkers` into `toolcache.FetcherConfig`)
- Modify: `README.md` (configuration table and the utility-content operations section)
- Modify: `docs/designs/2026-11-16-hephaestus-m2-utility-boot-design.md` §5.4 (the pool, the floor and reservations: one sentence each, describing the code)
- Test: `cmd/hephaestus/config_test.go`, `cmd/hephaestus/serve_test.go`

**Interfaces:**
- Consumes: Task 3's `DefaultFetchWorkers`, `MaxFetchWorkers` and `FetcherConfig.Workers`.

- [ ] **Step 1: Write the failing tests**

- `TestLoadConfig_Defaults` gains `FetchWorkers == 4`.
- `TestLoadConfig_Invalid` gains these rows: `HEPHAESTUS_FETCH_WORKERS=0` and `=17` give an error containing `HEPHAESTUS_FETCH_WORKERS 0: must be between 1 and 16` (respectively `17`); `=four` gives an error naming `HEPHAESTUS_FETCH_WORKERS`.
- A config test with `HEPHAESTUS_FETCH_WORKERS=2` gives `FetchWorkers == 2`.
- A serve test asserts the fetcher was built with the configured worker count. Use a dependency seam if `serveDeps` has one for the fetcher; otherwise observe it through two simultaneous `fetching` rows with `FetchWorkers: 2` against blocked upstreams. The builder chooses the cheaper of the two and says which.

- [ ] **Step 2: Run to verify they fail**

Run: `go test ./cmd/hephaestus/ -run 'LoadConfig|FetchWorkers' -race`
Expected: FAIL (`FetchWorkers` undefined, then the missing validation).

- [ ] **Step 3: Implement** the config key, the validation message `HEPHAESTUS_FETCH_WORKERS %d: must be between 1 and %d`, the serve wiring, and the README and design sentences. The README table row: `HEPHAESTUS_FETCH_WORKERS` | `4` | how many utility downloads run at once (1–16). The README also states the 32 KiB/s floor in one sentence.

- [ ] **Step 4: Run to verify they pass**

Run: `go test ./... -race`, then the gate.
Expected: PASS; gate exit 0.

- [ ] **Step 5: Commit**

```bash
git -c commit.gpgsign=false commit -m "feat: HEPHAESTUS_FETCH_WORKERS sets how many downloads run at once" -m "Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>"
```

### Task 6: e2e at the branch head

**Files:** none changed, unless the run shows a regression; then the fix goes in the task that owns it.

- [ ] **Step 1: Run the end-to-end test** in the background (about 6–7 min) and wait for the `exit=` line.

Run: `DOCKER_HOST=unix://$HOME/.colima/default/docker.sock rtk proxy sh -c 'mkdir -p build/e2e && task e2e > build/e2e/parallel-run.log 2>&1; echo "exit=$?" >> build/e2e/parallel-run.log'`
Expected: `e2e: 144 passed, 0 failed`, then `exit=0`.
