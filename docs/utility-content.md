# Utility Content

This page is the operations guide for the utility operating system cache: where `serve` keeps it, how it downloads, and what happens at startup and when the store is damaged.

`serve` downloads the utility operating systems itself and keeps them in the data directory (`HEPHAESTUS_DATA_DIR`). The internet is needed only for the first fetch of each entry: once an entry is `ready`, it boots with no upstream. Which entries exist, and how they are pinned, is in [Utility catalog](utility-catalog.md); the commands that request a fetch are in [Admin CLI](admin-cli.md); the health items and metrics are in [Observability](observability.md).

## Where It Lives

| Path | Holds |
|---|---|
| `<data>/store` | The content store, an OCI image layout. Each entry is one tag, `utility/<entry>-<arch>:<pin id>`, over one blob per extracted file. Talos boot files share the store under `talos/<schematic>-<arch>:<version>` tags (see [Talos Boot Assets](first-install.md#talos-boot-assets)). Blob files are read-only (0444) and never edited in place |
| `<data>/spool` | Partial downloads, `<pin id>-<object index>` with an `.etag` sidecar. A fetch resumes from them after a restart. Created 0700 at startup when missing |
| `<data>/git` | The git object store of the imported clusters, not part of the content store. [Install Talos](first-install.md) covers it |
| `<data>/store.corrupt-<UTC time>` | A store `serve` set aside at startup ([when the store is moved aside](#when-the-store-is-moved-aside)). Only the newest is kept |

## Durability

A blob is written to an ingest file, synced, renamed into place and its directory synced before the tag that names it is written, manifests and config blobs included, so an entry tagged before a power cut is whole afterwards. A blob already present at the right size is accepted only after its sha256 matches its digest. A file sync failure fails the push or the tag; a directory sync that the filesystem does not support is logged once at Warn and tolerated. This holds for content written by this release on; a right-size but zeroed blob that an older release tagged still passes the size check until it is fetched again.

## Artifact Route

A booted utility fetches its files from `GET /u/<entry>/<arch>/<version>/<path>` over plain HTTP on port 80, by design, with Range support. It serves a file only when the tag's layer has the digest and size the catalog pins for that path; any other layer, a missing one included, is a 404 that also puts a `ready` row back to `wanted`.

## Parallel Downloads

`HEPHAESTUS_FETCH_WORKERS` (default 4, 1 to 16) is how many entries `serve` downloads at once, utility and Talos downloads together; an invalid value stops `serve` at startup. The setting is read at startup, so a change takes a restart. An upstream that averages under 32 KiB/s over 2 minutes is cut off and the row fails and is retried with backoff, so a slow mirror cannot hold a worker for good.

## Free Space

A fetch needs free space on the spool volume: twice the object's size, held for as long as the download is in flight, and a 1 GiB margin the volume keeps free, counted once however many downloads run. So the downloads in flight need the sum of their own needs plus 1 GiB. A download starts only when the free space, less what the other downloads in flight hold and the margin, covers its own need. One that would fit alone but not beside the others waits: it goes back to `wanted` with no failed attempt, and is claimed again a poll later. One the volume cannot hold even alone fails and is retried with backoff. `HEPHAESTUS_FETCH_WORKERS=1` restores one download at a time. The data directory also holds the database, so `/health` warns on `disk:data_dir` below 10% free.

## When the Store Is Moved Aside

If `index.json` cannot be decoded or has the wrong type, if the `oci-layout` version is unsupported, if a digest is malformed, or if a manifest does not match its digest or is truncated, `serve` renames `<data>/store` to `store.corrupt-<UTC time>`, logs one `store was unusable; moved aside` line at Error with the new path, starts an empty store and refetches. Any other failure, a permission or I/O error included, stops `serve` with that error and moves nothing. An empty `index.json` is not corruption: the store resets it in place and keeps its blobs.

## Startup Reconciliation

Startup reconciliation runs after the database and the store are open and before anything serves or fetches. It logs one `utility reconciled` line with these counts:

| Count | What it did |
|---|---|
| `interrupted` | Rows that were `fetching` when the last process stopped go back to `wanted` |
| `deleted` | Rows of an entry or architecture the catalog no longer has are removed |
| `repinned` | A row at an older pin moves to `wanted` at the catalog's pin, with its attempts and error reset |
| `requeued` | A `ready` row whose tag or any layer is missing or the wrong size goes back to `wanted`, so a restored or repaired host refetches its cache unattended |
| `restored` | A `wanted` or `failed` row whose content is complete in the store becomes `ready` (a crash between tagging and the state update). A `broken` row stays broken |
| `spool_removed` | Spool files that no current pin owns are deleted. A sweep that cannot do it logs `utility spool sweep failed` at Warn and `serve` carries on: the leftovers are only disk |

## Garbage Collection

Garbage collection is restart-only. After reconciliation, `serve` untags every `utility/*` tag that is not a current entry's, logs `utility garbage collected` with the `untagged` count, and deletes the blobs no tag reaches. The store spares unreferenced blobs written in the last hour. Tags outside `utility/` are never touched. Nothing collects while `serve` runs, so a catalog change frees its disk on the next restart. A collection that fails logs `utility garbage collection failed` at Warn and `serve` carries on: the garbage is only disk. A reconciliation that fails stops `serve` with the error, and so does content the store cannot read (a permission or I/O error): only content the store lacks (an unknown tag, a missing or wrong-sized layer) requeues a `ready` row.

## Fetch Log Lines

Every state change the fetcher makes to a cache row (the claim, `ready`, `failed`, `broken`, and a reset to `wanted` after a database error) logs one Info line, `utility fetch`, with `entry`, `arch`, `pin`, `from`, `to`, `attempts`, `class` (`integrity` or `other` for a failure), `error`, `bytes` and `duration_ms`. Other changes log no per-row line: startup reconciliation logs the one `utility reconciled` summary, which gives counts only; boot-decision row 3's new `wanted` row is part of the boot request; and a requeue from boot-decision row 3 or `/u/` (lost content) is logged at Error as a `boot error`. An error that the fetcher could not store on a row logs `utility fetch loop error` at Warn and polling continues; a failed fetch never stops `serve`. A transient failure is retried after a backoff that starts at one minute, doubles and caps at one hour, and the tenth failure makes the row `broken`. An integrity failure (a hash that does not match, a redirect off https) makes it `broken` at once. `hephaestus utility fetch <entry>` re-arms a `failed` or `broken` row.

[← Back to README](../README.md)
