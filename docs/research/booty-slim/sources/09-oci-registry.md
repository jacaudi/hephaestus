# Built-in OCI pull-through registry: libraries, Talos mirror behaviour, recommendation

Date: 2026-11-12. Method: read-only. I made shallow clones at pinned tags in `/tmp/registry-research/`,
read the source, and fetched the official Sidero docs. I also sent a few anonymous `GET`/`HEAD`
requests to `factory.talos.dev` on 2026-11-13 UTC. Nothing was built or run.
**UNVERIFIED** marks claims I could not check in source or docs. **INFERRED** marks my own conclusions.

Pinned sources:

| Repo | Tag | Release date | Last commit on default branch |
|---|---|---|---|
| siderolabs/talos | v1.14.6, v1.15.0-beta.0 | | |
| containerd/containerd (Talos `go.mod:61` pins v2.3.7) | v2.3.7 (3 files fetched raw) | | |
| siderolabs/image-factory | v1.8.0 | | |
| project-zot/zot | v2.1.23 | 2026-11-02 | 2026-11-12 |
| distribution/distribution | v3.1.2 | 2026-09-24 | 2026-11-12 |
| google/go-containerregistry (ggcr) | v0.22.1 | 2026-09-04 | 2026-10-26 |
| olareg/olareg | v0.2.2 | 2026-06-24 | 2026-10-23 |
| oras-project/oras-go | v3.0.0 | 2026-11-01 | n/a |
| regclient/regclient (client only, not inspected) | v0.11.7 | 2026-10-21 | 2026-11-09 |

Dates come from `gh api repos/<r>/releases` and `/commits`. Every repo is Apache-2.0 per the GitHub
license field. None of the five registry modules has `import "C"` in its own code. Transitive deps
were not audited for cgo.

---

## 0. Headline findings

1. **VERIFIED: installer pulls honor registry mirrors** at v1.14.6 and v1.15.0-beta.0. This covers
   all four paths: the legacy `.machine.install.image` install, `UnattendedInstallConfig`, staged
   upgrade, and `talosctl upgrade`. All of them call `RunInstallerContainer` → `image.PullWithRetriesAndTimeout`
   with `crires.RegistryBuilder(...)`. That resolver is built from the `RegistriesConfig` resource,
   which is derived from the **active** machine config (section 2).
2. **Maintenance mode: the mirror config in the applied config is in effect for the install.**
   `apply-config` runs `SetConfig` (ActiveID), which regenerates `RegistriesConfig`. Only after that
   does `LoadConfig` finish and the install run. The mirrors you ship in the same config document
   are used for the installer pull. There is a small ordering race (section 2.4).
3. **Talos matches mirrors by exact registry host, else `*`.** There is no path-prefix keying. One
   service can still serve every upstream: give each upstream host its own mirror entry, all pointing
   at the same service with a per-upstream path and `overridePath: true`. containerd also sends
   `?ns=<upstream-host>` on every mirror request, so a `*` entry can be routed by query parameter.
4. **Signatures: `factory.talos.dev` serves cosign signatures as OCI referrers** (sigstore bundle
   v0.3). The `sha256-<hex>` fallback tag also exists, and there is no `.sig` tag. Talos tries
   referrers, then the bundle tag, then `.sig`, all through the mirror. A cache that passes through
   either referrers or arbitrary tags keeps verification working.
5. **No candidate library does the whole job.** Every candidate fails at least one of these:
   in-process `http.Handler`, multiple upstreams, referrers, pin-driven GC, and light deps.
   - zot does all of it except embedding and dependency weight: 94 direct and 475 indirect deps, own listener, build tags.
   - distribution has one upstream per instance and no referrers.
   - ggcr and olareg have no proxy at all.
   - **Recommendation:** write a thin read-only pull-through handler (about 1 kLOC, INFERRED).
     Use `oras-go/v3` for the upstream client and the on-disk OCI-layout store, which provides
     `Tag`/`Untag`/`Delete`/`GC`. GC then becomes: untag what is not pinned, then `Store.GC()`.

---

## 1. Library candidates

### 1.1 Comparison

| | zot v2.1.23 | distribution v3.1.2 | ggcr `pkg/registry` v0.22.1 | olareg v0.2.2 | oras-go v3.0.0 (building block) |
|---|---|---|---|---|---|
| In-process `http.Handler` | Partial. `api.NewController`+`Init`+`Run` start their own `http.Server` | Yes, `handlers.NewApp(ctx,cfg)` is an `http.Handler` | Yes, `registry.New(...)` | Yes, `olareg.New(cfg)` / `ServeHTTP` | No server; client + store only |
| Pull-through | Yes, `sync` extension `onDemand: true` | Yes, `proxy.remoteurl` | **No** | **No** (`config/config.go:36` "TODO: proxy settings") | Build it: `registry/remote` + `oras.Copy`/`ExtendedCopy` |
| Multiple upstreams | Yes, `registries[]`, each with `content[].prefix/destination` | **No**, one `Proxy` per app; one app per upstream | n/a | n/a | Yours to route |
| Filesystem storage | Yes (dedupe, bbolt meta) | Yes, `filesystem` driver | Blobs only (`NewDiskBlobHandler`); **manifests in memory** | Yes, `StoreDir` (OCI layout per repo) | Yes, `content/oci.Store` (OCI layout) |
| GC/delete you can drive | DELETE API + GC; retention policies by tag patterns/age, not external pins | `storage.MarkAndSweep` (wants read-only or stopped registry); proxy TTL default 7 d | Blob `Delete` only | DELETE API (off by default) + periodic GC config | `Untag`, `Delete`, `GC(ctx)` in Go |
| OCI 1.1 referrers | Yes, also syncs referrers and legacy cosign tags | **No** (no referrers code in `registry/`) | Opt-in `WithReferrersSupport` | Yes, on by default | Client `Repository.Referrers`; store `Predecessors` |
| Cosign passthrough | Yes, `SyncLegacyCosignTags` (default true), referrer sync | `.sig`/`sha256-` tags only (plain tags) | n/a | n/a | `ExtendedCopy` copies referrers |
| Upstream auth | Credentials file, ECR/GCP/OAuth2 helpers | `username/password` or `exec` helper | n/a | n/a | `auth.Client` |
| Multi-arch index | Yes, `platforms` filter for periodic sync (on-demand ignores per-content platforms) | Index proxied; children fetched on demand | stores what is pushed | `SparseIndex` option | `oras.Copy` with platform selection |
| Direct / indirect requires | **94 / 475** (trivy, aws-sdk, notation, cosign, gqlgen, redis, bbolt) | 38 / 72 (aws-sdk, azure, gcs, redis, otel; handlers imports redis + otel) | 12 / 25 module-wide; pkg itself is light | **4 / 2** | 6 / 3 (go-digest, image-spec, x/sync, toml, yaml, ProtonMail/go-crypto) |
| Go toolchain | `go 1.27.0`, Makefile sets `GOEXPERIMENT=jsonv2` | | | `go 1.25.0` | `go 1.26.0` |
| Stated purpose | Production registry | Production registry | "initial focus on tests" | Small embeddable registry | Library |

### 1.2 Per-candidate notes

**zot** (`zotregistry.dev/zot/v2`):
- **Embedding.** `pkg/api/controller.go:75` `NewController`, `:351` `Init`, `:158` `Run`. `Run` builds the
  gorilla router (`:165-191`, `NewRouteHandler(c)`), then creates and serves its own `http.Server` (`:200-213`).
  - You can embed it in-process with zot on its own port.
  - Mounting zot under your own mux means copying `Run`'s router setup. That uses exported names,
    but there is no embedding contract (INFERRED).
- **Build tags.** Extensions need build tags: `pkg/extensions/extension_sync.go:1` `//go:build sync`,
  and the Makefile `EXTENSIONS ?= sync,search,...` (`Makefile:87`). Your binary must build with `-tags sync`.
- **Sync config.**
  - `pkg/extensions/config/sync/config.go:58-80`: `URLs`, `OnDemand`, `OnlySigned`, `SyncLegacyCosignTags`, `SyncTimeout`.
  - `:311-321`: `Content{Prefix, Destination, StripPrefix}`.
  - `pkg/extensions/sync/content.go:115` `GetRepoSource` maps a local repo back to the upstream name.
    That makes `prefix: "**", destination: "/factory.talos.dev"` plausible for namespacing per upstream.
    I did not test this.
- **Retention** (`pkg/api/config/config.go:44-66`) is policy-by-pattern. To follow external pins
  you would issue DELETEs and let GC run.
- **Cost.** It is the biggest dependency graph by far, and the module requires Go 1.27. zot builds
  with `CGO_ENABLED=0` (`Makefile:203`). It fits as a sidecar binary, not as a library.

**distribution v3**:
- **Proxy config.** `configuration/configuration.go:697-722` has one `Proxy{RemoteURL, Username,
  Password, Exec, TTL, CacheWriteTimeout}` per configuration. `registry/proxy/proxyregistry.go:40`
  `NewRegistryPullThroughCache` sets a TTL scheduler with a 7-day default, and 0 disables it.
- **Multiple upstreams** means N `handlers.NewApp` instances (`registry/handlers/app.go:101`; `ServeHTTP` at `:694`),
  each with its own storage root. You route between them yourself and rewrite `/v2/<upstream>/<repo>` to `/v2/<repo>`.
- **Panics.** `NewApp` panics on config errors (20 `panic(` in `:101-400`).
- **GC.** `storage.MarkAndSweep(ctx, driver, namespace, GCOpts{DryRun, RemoveUntagged})`
  (`registry/storage/garbagecollect.go:20-34`). The docs require read-only mode or a stopped registry
  during GC (`docs/content/about/garbage-collection.md:83`).
- **Referrers.** A grep for "referrer" outside `docs/` and `vendor/` finds nothing, so there is no
  referrers API. Talos then falls back to the `sha256-<hex>` tag, which proxies as a normal tag.
- **Cosign.** It works only through that fallback.

**ggcr `pkg/registry`**:
- The package doc (`pkg/registry/registry.go:15-23`) says "initial focus on tests" and "low flightmiles".
- Manifests live in a Go map (`pkg/registry/manifest.go:47-51`), so they are lost on restart. Blobs
  can go to disk (`blobs_disk.go:32`).
- It has no proxy. Not a candidate for the server.
- `ggcr/pkg/v1/remote` remains a credible upstream client.

**olareg**:
- `olareg.New(config.Config)` returns `*Server` with `ServeHTTP` (`olareg.go:32,137`).
- `StoreDir` uses an OCI layout per repo.
- GC options: `Untagged`, `ReferrersDangling`, `GracePeriod`, `Frequency`.
- Referrers are enabled by default. DELETE is disabled by default (`config/config.go`).
- Only 4 direct deps.
- No proxy (`config/config.go:36` TODO). Its storage is `internal/`, so you could only fill it by
  pushing over HTTP to the in-process handler. That works but is awkward (INFERRED).

**oras-go v3** (not a server, but the best building block):
- `content/oci.Store` provides `Tag` (`content/oci/oci.go:370`), `Delete` (`:303`) and `GC` (`:703`).
  - The GC doc at `:690-702` says unreachable blobs younger than 1 h are kept, and GC "does not
    coordinate with concurrent writers" other than the same Store instance.
  - Reference strings are not validated beyond non-empty (`:1037-1044`), so a full
    `factory.talos.dev/metal-installer/<s>:v1.14.6` works as the tag. One store then dedupes blobs across upstreams.
- `registry/remote.Repository.Referrers` is at `registry/remote/repository.go:835`.
- v3.0.0 shipped 2026-11-01 with a `MIGRATION_GUIDE.md`. The API is new, so pin it.

**Others considered.**
- Harbor and Spegel are services, not libraries. Spegel's design shows the `ns=`-routing idea (INFERRED).
- regclient is the client counterpart of olareg. I did not inspect it (UNVERIFIED for API details).

---

## 2. Do Talos installer pulls honor mirrors? **VERIFIED**

Line numbers are `@v1.14.6`. Where `@v1.15.0-beta.0` differs, it is given as "(1.15b0 :N)". These
files are byte-identical between the two tags: `internal/pkg/containers/image/{resolver,pull}.go`,
`internal/pkg/install/install.go`, `internal/app/machined/pkg/controllers/cri/registries_config.go`,
and `pkg/machinery/config/types/cri/registry_mirror.go`.

### 2.1 Common pull path

- `internal/pkg/install/install.go:49` `RunInstallerContainer(..., registryBuilder image.RegistriesBuilder, ...)`.
  At `:116` it pulls with `image.PullWithRetriesAndTimeout(ctx, registryBuilder, ...)` into the
  **system** containerd namespace (`:70`).
- `internal/pkg/containers/image/pull.go`:
  - `:164` calls `registryBuilder(ctx)`, and `:169` runs `resolver := NewResolver(registriesConfig)`.
  - `:172` runs `verify.ImageSignature` with the same resolver.
  - `:189` passes `containerd.WithResolver(resolver)`, and `:235` calls `client.Pull`.
  - The builder is re-invoked on each retry (`:100`). Retries back off exponentially for up to 20 min
    (`image.go:18`), with at most 5 not-found retries (`image.go:37`).
- `pkg/machinery/resources/cri/cri.go:41-57` (1.15b0 :43): `RegistryBuilder` watches the `RegistriesConfig` resource.
- `internal/app/machined/pkg/controllers/cri/registries_config.go`:
  - `:72` reads `MachineConfig` **ActiveID**.
  - `:90-103` copies `RegistryMirrorConfigs()` (endpoints, `overridePath`, `skipFallback`).
  - `:105-120` copies auths and TLS.
  - `:123-136`: when Talos Image Cache is Ready, it prepends `http://<registryd>` to every mirror and
    adds `*`. Expect that extra endpoint ahead of yours if Image Cache is on.
- `pkg/machinery/config/container/container.go:1021-1039`: the legacy `.machine.registries.mirrors`
  and `RegistryMirrorConfig` documents are merged. On the same name, the document wins.

### 2.2 Every installer entry point uses it

| Path | Call site |
|---|---|
| Legacy install (`.machine.install.image`, default `factory.talos.dev/metal-installer/<default-schematic>:<ver>`, `pkg/images/images.go:84-108`) | `runtime/v1alpha1/v1alpha1_sequencer_tasks.go:1490-1492` image, `:1533-1540` `RunInstallerContainer(..., crires.RegistryBuilder(...))` (1.15b0 :1495, :1538-1545) |
| `UnattendedInstallConfig` | `controllers/runtime/unattended_install.go:76-83` (both tags). When the install sequence is skipped (`v1alpha1_sequencer.go:161`), the image is `doc.InstallerImage()`, or if empty it is derived from the boot entry's Image Factory schematic: host from `ImageFactorySchematic.APIURL`, `:261-268`, `:317-334` |
| Staged upgrade | `v1alpha1_sequencer_tasks.go:1601-1607` (1.15b0 :1606-1612) |
| `talosctl upgrade` | API pre-pull `internal/app/machined/internal/server/v1alpha1/v1alpha1_server.go:513-519` → `install.PullAndValidateInstallerImage` (`internal/pkg/install/pull.go:29,42`) with `RegistryBuilder` (1.15b0 :534). Then the `Upgrade` task `v1alpha1_sequencer_tasks.go:1259-1265` (1.15b0 :1264-1270) |
| `ContainerConfig` images (relevant for caching them) | `controllers/containers/image.go:424` (1.15b0 :444) `registryBuilder: cri.RegistryBuilder(ctrl.State)` |

The docs agree in substance. The air-gapped guide lists "Talos `installer` images" among the images
that registry mirrors redirect (docs.siderolabs.com/talos/v1.13/platform-specific-installations/air-gapped).
The pull-through-cache page says mirror config "is honored by Talos Linux itself and automatically
propagated to CRI".

### 2.3 `skipFallback`

It is respected. `internal/pkg/containers/image/resolver.go:120-137` appends `https://<default host>`
as the last endpoint only when `!reg.SkipFallback()`. containerd tries the hosts in order. It moves to
the next host on a transport error (`resolver.go:320-330` @containerd v2.3.7) and on a 404 (`:334-341`).
Without `skipFallback`, a mirror outage silently falls through to the internet. With
`skipFallback: true`, the install fails closed.

### 2.4 Maintenance mode: when does the mirror config take effect?

1. `apply-config` in maintenance runs `v1alpha1_server.go:197` `inMaintenance`, and `:219-221` coerces
   REBOOT to NO_REBOOT. Then `:301-306` calls `SetPersistedConfig` and `SetConfig` (1.15b0 :215, :319-324).
2. `v1alpha1_runtime.go:145-146` `SetConfig` writes `MachineConfig` ActiveID. `RegistriesConfigController`
   re-runs, because ActiveID is its input (`registries_config.go:35-40`).
3. `controllers/config/acquire.go:717-747` `stateMaintenance` sees a `CompleteForBoot` config, leaves
   maintenance, and `stateDone` (`:782`) writes `AcquireConfigStatus`. That unblocks the `LoadConfig` task
   (`v1alpha1_sequencer_tasks.go:100-116`), after which the `Install` sequence runs
   (`v1alpha1_sequencer.go:151-208`).

Result: the mirrors in the applied config are live before the installer pull.

- **Race (INFERRED, lab-test).** `RegistryBuilder` returns whatever `RegistriesConfig` exists, because
  `StateWatchFor` returns immediately on an existing resource. So a pull that started before the
  controller reconciled could see the pre-apply (empty) mirrors.
- **Mitigations.**
  - The install first blocks on `WaitForImageCache` (`:1497`).
  - The builder is re-read on each retry.
  - With `skipFallback: true`, a stale read fails closed and the retry picks up the mirror.
- **Same path for other config sources.** A config fetched via `talos.config=` (cmdline or platform
  state) goes through the same `ConfigSetter.SetConfig` (`acquire.go:219`).

### 2.5 Signature verification goes through the mirror

`internal/pkg/containers/image/verify/verify.go:46-115`: verification runs only when an
`ImageVerificationConfig` rule matches; with no matching rule, the pull is allowed.
`verify/internal/cosign/cosign.go:86-123` tries the following, all through the mirror resolver and
`TagFetcher`:

1. Referrers (`FetchReferrers`, sigstore bundle v0.3).
2. The `sha256-<hex>` bundle tag.
3. The `sha256-<hex>.sig` tag.

The resolver advertises `HostCapabilityReferrers` for every mirror (`resolver.go:105`).

Image Factory v1.8.0:
- It routes `/v2/<image>/<schematic>/referrers/<digest>` and proxies `siderolabs/*` referrers
  (`internal/frontend/http/oci/handler.go:52-140`).
- The GSA signer stores "OCI referrer bundle" signatures (`internal/image/signer/gsa.go:74`).

Live probe of `factory.talos.dev/v2/metal-installer/376567…b4ba`, tag `v1.14.6`:
- The tag is an OCI index (amd64 and arm64, docker v2 child manifests) with digest `sha256:125325c7…a569`.
- `referrers/<digest>` returns one `application/vnd.dev.sigstore.bundle.v0.3+json` manifest.
- The `sha256-<hex>` tag returns 200.
- `sha256-<hex>.sig` returns 404.

---

## 3. Mirror keying, path prefixes, TLS

### 3.1 Keying

- **Lookup order.** `resolver.go:140-163`:
  1. An exact match on the registry host. containerd passes `refspec.Hostname()`, including any port.
  2. Otherwise `*`.
  3. Otherwise the default host (`docker.io` → `registry-1.docker.io` via `docker.DefaultHost`).
- **Document schema.** `RegistryMirrorConfig` `name` is "the first segment of image identifier"
  or `*` (`registry_mirror.go:55-64`). Factory images key on `factory.talos.dev`, `ghcr.io/siderolabs/*`
  on `ghcr.io`, and Hub on `docker.io`.
- **Path handling.** The endpoint path becomes `RegistryHost.Path`. It gets `/v2` appended unless
  `overridePath` is set, or unless the path already ends in `/v2` (`resolver.go:76-86`).
- **Request URL.** containerd builds `"/" + host.Path + repository + ...` (`core/remotes/docker/resolver.go:564` @v2.3.7).

**One service, many upstreams.** Mirror selection is per host, not per path prefix: a single mirror
entry cannot fan out by repo prefix. One endpoint can still serve every upstream in either of two ways:

- **A. Path prefix per upstream (documented pattern, Harbor-style).** This is what the docs show:
  `url: http://harbor/v2/proxy-docker.io`, `overridePath: true` (pull-through-cache page, v1.13/v1.15).
  The service sees `/v2/factory.talos.dev/metal-installer/<s>/manifests/v1.14.6`, which is the standard
  OCI path for repo `factory.talos.dev/metal-installer/<s>`. Its first segment selects the upstream.

  ```yaml
  apiVersion: v1alpha1
  kind: RegistryMirrorConfig
  name: factory.talos.dev
  endpoints:
    - url: http://booty.lan:5000/v2/factory.talos.dev
      overridePath: true
  skipFallback: true
  ---
  apiVersion: v1alpha1
  kind: RegistryMirrorConfig
  name: ghcr.io
  endpoints:
    - url: http://booty.lan:5000/v2/ghcr.io
      overridePath: true
  ```

- **B. Route by `ns`.** containerd appends `?ns=<ref host>` whenever the mirror host differs from the
  ref host (`resolver.go:609-616`, `registry.go:85-92` @v2.3.7). This applies to manifest HEAD/GET, blob
  fetch (`fetcher.go:321-395`) and referrers (`referrers.go:116,135`). A single
  `name: "*"` → `http://booty.lan:5000` entry therefore works if the handler keys on `ns`.
  - **Caveat.** This is containerd behaviour, not the OCI spec. Prefer A, and accept B as a fallback
    for clients that hit the bare `/v2/<repo>`.

### 3.2 Plain HTTP vs TLS, private CA

- **Schemes.** `http://` and `https://` are both accepted (`registry_mirror.go:141-150`); the docs say
  "should explicitly include the protocol". Plain HTTP works with no extra config.
- **Lookup keys.** TLS and auth are looked up by the **endpoint** `host:port` (`resolver.go:57-58`), so
  one `RegistryTLSConfig`/`RegistryAuthConfig` named `booty.lan:5000` covers every mirror entry above.
  Setting a TLS config on an `http://` endpoint is an error (`resolver.go:60-62`).
- **`RegistryTLSConfig`** (`pkg/machinery/config/types/cri/registry_tls.go`) has `name` (endpoint host),
  `ca`, `clientIdentity` (mTLS) and `insecureSkipVerify`.
  - When `ca` is set, the root pool is **replaced** with that CA alone (`pkg/machinery/resources/cri/registries_config.go:180-183`),
    even though the doc text says "add".
  - Only when no CA is given does it use the system bundle (`resolver.go:70-73`).
- **`TrustedRootsConfig`** (`pkg/machinery/config/types/security/trusted_roots.go:18`) is the alternative.
  - `TrustedRootsController` appends the extra roots to `/etc/ssl/certs/ca-certificates`
    (`internal/app/machined/pkg/controllers/secrets/trusted_roots.go:59-88`).
  - Every registry transport re-reads that file (`pkg/httpdefaults/httpdefaults.go:19-33`, `tls.go:26-58`).
  - That trusts the private CA system-wide (registry, discovery, and so on) without a per-registry document.
  - The file is written asynchronously, so a first pull could race it (INFERRED, lab-test).
- **Timeouts that shape the server.**
  - There is no overall HTTP timeout, but a 90 s **idle** stall guard (`resolver.go:206-213`).
  - The client forces HTTP/1.1 so blob reads can resume with Range (`:219-227`).
  - Consequence: the cache must start streaming within 90 s. Downloading a large layer in full before
    the first byte can trip it.

---

## 4. Recommendation

### 4.1 Choice

Build a **thin, read-only pull-through handler in the service** on top of `oras-go/v3`. Keep zot as
the fallback if the scope grows into a general-purpose registry (push, UI, search). This choice holds
only if the handler is truly read-only and serves just what Talos and containerd ask for. I estimate
that at about 1 kLOC plus tests (INFERRED).

| Need | How oras-go v3 + own handler meets it |
|---|---|
| `http.Handler` on the service's mux, one binary, cgo-free | Own code. oras-go has 6 direct deps and no cgo in the module |
| Multiple upstreams | Route on the first path segment (`/v2/<upstream-host>/...`), with `?ns=` as fallback. An allow-list of upstream hosts lives in config |
| Filesystem | One `content/oci.Store` at `$DATA/oci/`. Tag = full ref (`factory.talos.dev/metal-installer/<s>:v1.14.6`). Blobs are deduped across upstreams |
| Pin-driven GC | For each cached ref with no pin, call `Store.Untag(ref)`, then `Store.GC(ctx)`. Same Store instance, so it is safe in-process (oci.go:690-702) |
| Referrers / cosign | On a manifest miss, copy the subject manifest plus referrers (`oras.ExtendedCopy`, or `Repository.Referrers` + copy). Serve `GET /v2/<repo>/referrers/<digest>` from `Store.Predecessors`. Plain tags (`sha256-<hex>`) proxy like any tag |
| Multi-arch | Store the index as-is. Fetch children on demand, or pre-copy only the pinned arch |
| Upstream auth | `registry/remote/auth.Client` per upstream (factory and ghcr are anonymous for public images) |

Handler surface: `GET /v2/`, `HEAD|GET manifests/<tag|digest>`, `HEAD|GET blobs/<digest>`
(with `Range`), `GET referrers/<digest>`, and `GET tags/list` (optional). Return 405 for push and delete.

### 4.2 How the cache fills, and how GC follows pins

1. **Pin-first prefetch** (the normal path). When the service pins (Talos version, schematic), it
   enqueues `oras.ExtendedCopy` from the upstream ref to the local store, filtered to the node arch,
   with referrers. It records `cache_entries(ref, digest, pin_id, fetched_at)` in SQLite or Postgres.
   Installs then never wait on the upstream.
2. **On-demand miss.**
   - Manifest: resolve upstream, fetch the manifest, store it, then serve.
   - Blob: **tee-stream** the upstream body to the client and into a temp file. Commit by digest on
     completion, so the 90 s idle guard never fires.
   - Coalesce concurrent misses per digest with singleflight.
   - Record the ref as `unpinned`, with a TTL.
3. **GC job.** It reuses the file-cache retention scheduler:
   1. Compute the keep-set = refs referenced by any pin, plus unpinned refs younger than their TTL.
   2. `Untag` every other ref.
   3. Call `Store.GC`. It walks reachability, so children, layers and referrers of kept refs survive.
   4. Delete the DB rows.
   - Run it under the same lock as the cache writer, because oras GC does not coordinate with other writers.
   - The digest entries oras adds (`oci.go:395-400`) count as untagged. GC keeps them only if reachable from a tag, or if they are referrers whose subject chain reaches one (`oci.go:895-945`). `Untag(ref)` alone is enough.
4. **Talos side.** Generate one `RegistryMirrorConfig` per upstream, pointing at the service with
   `overridePath: true`, and default `skipFallback: true` when the node has no internet. Ship
   `TrustedRootsConfig` if the service is HTTPS with a private CA, or use plain HTTP on the
   provisioning LAN.

**If zot is chosen instead:**
- Run it as a child process or sidecar, not as a library.
- Config: `sync.registries[]` per upstream, with `onDemand: true` and `content: [{prefix: "**", destination: "/<upstream-host>"}]`.
- Drive retention by sending `DELETE /v2/<repo>/manifests/<digest>` for unpinned refs, then let zot GC.
- It costs a second binary, a Go 1.27 / `jsonv2` toolchain, and about 570 modules.

### 4.3 Lab tests still needed

1. **Maintenance-mode race.** Fresh node on 1.14.6: apply a config that contains only `RegistryMirrorConfig`
   (with `skipFallback: true`) and no internet route. Confirm the installer pull's first attempt hits
   the mirror (service access log), not a stale empty config.
2. **`UnattendedInstallConfig` with no `installerImage`.** Confirm the derived host is `factory.talos.dev`
   for PXE assets fetched from the factory, and that the mirror catches it.
3. **`talosctl upgrade --image factory.talos.dev/metal-installer/<s>:v1.15.x`.** The pre-pull goes
   through the mirror, and so does the `Upgrade` task (the image is cached, so the second pull is skipped).
4. **`ImageVerificationConfig` keyless rule for the factory identity.** Verification succeeds against
   the cache via referrers. Then disable the referrers route and confirm the `sha256-<hex>` tag fallback works.
5. **`overridePath` plus the `ns` parameter.** Confirm the exact request lines containerd sends, for both
   the path-prefix and the `*` setups, including blob `Range` resume over HTTP/1.1.
6. **HTTPS with `RegistryTLSConfig.ca` vs `TrustedRootsConfig`.** Confirm the first pull after
   apply-config does not race the `/etc/ssl` file write.
7. **Talos Image Cache enabled.** registryd is prepended. Check that it does not mask the service mirror
   for images missing from the local cache.
8. **GC under load.** Run GC while a node streams an installer: no 404 mid-pull, and the 1 h grace
   protects in-flight blobs.

Out of scope here and UNVERIFIED: whether `docker.io` anonymous rate limits matter for
`ContainerConfig` images. Also regclient's server-side capabilities.
