# hephaestus: parallel utility downloads and clean fetch errors

Date: 2026-11-17. Status: approved by the owner in conversation (2026-11-17). Size: a small change on top of M2 (main `5603d28`).

## Intent

A single slow upstream must not hold back every other utility. Today the fetcher downloads one entry at a time, and a server that trickles a byte just inside the 60 s idle timeout occupies that one worker forever. Every other queued utility stays "not ready", so machines flagged for them boot normally instead of getting their utility. The owner wants several downloads at once, and machines booting their utilities without waiting on a stuck download.

The same change fixes a security-audit lead (re-run on main, record S1, needs validation): a stored fetch error can keep C1 terminal-control characters taken from upstream text (a redirect host, a status line). `--json` CLI output and the JSON server logs then print them raw. Text-mode CLI output is already escaped.

## What is true today (read from the code)

- Boots already run in parallel: every `/boot` and `/u/` request has its own goroutine, and the fetcher holds no database transaction while it downloads. A download never blocks a boot.
- SQLite runs on one connection (`internal/db/db.go`, `SetMaxOpenConns(1)`, `_txlock=immediate`), so database steps take turns. Each is milliseconds, so this is not a bottleneck.
- `Fetcher.Run` (`internal/toolcache/fetcher.go`) loops `runOnce`: claim one row, download, verify, extract, record.
- When the database refuses to record an outcome, the fetcher holds it. If the retry fails, `Repo.ResetFetching` moves **every** `fetching` row to `wanted`. With several workers, that would take other workers' rows from under them.
- `claimFetch` (`internal/fleet/utility_repository.go`) picks the best claimable row, then moves it with a conditional update (`WHERE state = <state seen>`). On Postgres a concurrent claim of the same row matches no row and returns "nothing claimed".
- The free-space check is per download: free space must be at least 2× the object plus a 1 GiB reserve (`spaceReserve`). It does not know about other downloads in flight.
- `fleet.CleanText` removes NUL and invalid UTF-8 only. `storedCause` applies it, then caps the text at 1 KiB. The fetch event's `error` field is logged straight from the Go error.

## Goals

1. Up to N downloads at once. N defaults to **4** and is set with `HEPHAESTUS_FETCH_WORKERS` (owner, 2026-11-17).
2. A download that stays slower than **32 KiB/s, averaged over 2 minutes**, is cut off as a retryable failure. It backs off like any network failure, then retries, possibly via another mirror. It never holds a worker indefinitely (owner accepted the proposed value).
3. Free-space checks account for every download in flight, so N downloads cannot together overfill the volume.
4. A held outcome's reset touches only its own row.
5. A fetch error, wherever it ends up (the stored `last_error`, the fetch event, `--json`, the logs), holds no non-printable characters.

## Non-goals

- Per-host connection limits. With N = 4 across seven entries this is not needed; revisit only if a mirror network objects.
- Making the throughput floor or its window configurable. Two constants; YAGNI.
- Changing how boots or `/u/` serve. They are already concurrent.
- A different SQLite connection model.

## Anti-goals

- Two workers downloading the same `(entry, arch)` at once.
- A worker's held-outcome reset changing a row another worker owns.
- A download starting when the volume cannot hold it alongside the others already reserved.
- Any change to M1 behaviour, for example `CleanText` on serial and product.

## Approaches considered

- **A. A worker pool plus a throughput floor (chosen).** N goroutines each run today's claim → download → record loop. A sliding-window rate check cuts off trickles. This uses the existing claim, backoff and failure classes unchanged; the new parts are the pool, the floor, reservations and the per-row reset.
- **B. Parallelism only.** N workers with no floor. It is simpler, but a trickling upstream still pins a worker forever, and N trickles stall everything again. It does not solve the problem.
- **C. Floor only.** One worker with a throughput floor. A stuck download is cut off after about 2 minutes, but downloads still run strictly one after another: a 1.4 GB ISO holds up a 400 KB memtest. It does not give parallel downloads.

## The design

- **Config.** `HEPHAESTUS_FETCH_WORKERS` is an integer from 1 to 16, default 4, validated in `loadConfig`. An invalid value refuses to start serve with a clear message (12-factor: a misconfigured service never starts). It reaches `toolcache.FetcherConfig` as `Workers`.
- **Pool.** `Fetcher.Run` starts `Workers` loops over a shared claim. Each loop polls as today; a successful claim triggers an immediate re-poll. `Run` still returns only when its context is done (gate-1 B5), after every loop has returned. Per-worker state (today's single `held`) becomes per-loop. Metrics and events are unchanged except for being concurrent.
- **Claim exclusivity.** `claimFetch` stays a conditional update. If the update matches no row because another worker won it, the claim tries the next candidate in order instead of reporting "nothing claimed". Two workers therefore never get the same row, and a lost race does not idle a worker for a whole poll. This must hold on SQLite and Postgres.
- **Throughput floor.** A rate check around the body reader: once a transfer has run at least 2 minutes, it fails when the bytes in the last 2 minutes are under 32 KiB/s × 120 s. The failure is the default (retryable) class, with a cause naming the rate. The 60 s idle timeout stays. Both run on the injected clock so tests do not sleep.
- **Free-space reservations.** The fetcher keeps an in-memory sum of reservations (2× object + 1 GiB each) for the downloads in flight. A new download needs `free − reserved by others ≥ its own need`. A reservation is released when its download ends, whatever the outcome. Only one serve process writes a store, which is the existing single-writer rule.
- **Per-row reset.** The held-outcome retry resets only its own row, moving it from `fetching` to `wanted` only while it is still `fetching` at its pin. This replaces the call to the all-rows `ResetFetching` there. Startup reconciliation keeps `ResetFetching`, because at startup nothing else is in flight.
- **Clean fetch errors.** One function in `fleet`, for example `fleet.PrintableText`, keeps only printable characters. It drops C0 and C1 controls, bidi controls and zero-width characters, plus anything else that is not `unicode.IsGraphic`, and keeps ordinary spaces. The fetcher builds each failure's cause once, `fleet.PrintableText(err.Error())`, and uses it for both `FetchFailure.Cause` and the logged event's error text. `storedCause` applies it too, before the 1 KiB cap, as defence at the repository boundary. `CleanText`, which M1 uses for serial and product, is unchanged.
- **Docs.** The README lists `HEPHAESTUS_FETCH_WORKERS` and describes the floor. The M2 design §5.4 gains the pool, the floor and the reservation rule.

## Tests

The builder writes each test first and shows it failing.

- Claim exclusivity on both databases: two concurrent claims over two wanted rows return two different rows. Over one row, exactly one claim succeeds and the other returns nothing.
- Pool: with N = 2 and two upstreams, one trickling, the fast entry reaches `ready` while the slow one is still running. That is the stall the owner described.
- Floor: a trickle under 32 KiB/s fails at the 2-minute mark on an injected clock, in the default class. A steady transfer above the floor completes.
- Reservations: with free space for one large download only, a second large download fails the free-space check while the first is in flight, and succeeds after it ends.
- Per-row reset: a held outcome's reset leaves another worker's `fetching` row untouched.
- Clean errors: an upstream cause carrying C1 (U+009B), a bidi override (U+202E) and a zero-width space (U+200B) is stored and logged without them, and `utility list --json` shows the cleaned text.
- Config: values 0, 17 and non-numeric refuse to start; unset gives 4.

## Left to the builder

The names of the rate-check type and the reservation helper; whether the pool is an `errgroup` or a `sync.WaitGroup`; the exact cause wording. The sanitizer keeps ordinary spaces and drops tabs.
