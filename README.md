# hephaestus

hephaestus gets bare metal from power-on to a correctly configured Talos node; once caches are warm, no boot or install step depends on the internet or any cloud service, and no secret sits anywhere unencrypted.

## Quick Start

```sh
task build
```

`hephaestus serve` reads its configuration from the environment and refuses to start if any value is missing or invalid (every problem is reported together).

| Variable | Default | Meaning |
|---|---|---|
| `HEPHAESTUS_FQDN` | required | Name the boot HTTPS certificate covers |
| `HEPHAESTUS_PROVISIONING_INTERFACE` | required | Interface proxyDHCP listens on; it must exist and hold the provisioning address |
| `HEPHAESTUS_PROVISIONING_ADDRESS` | required | This host's IPv4 address on the provisioning subnet |
| `HEPHAESTUS_PROVISIONING_SUBNET` | required | Provisioning subnet, as an IPv4 CIDR prefix |
| `HEPHAESTUS_TLS_MODE` | `file` | `file` or `proxy`; see [TLS](#tls) |
| `HEPHAESTUS_TLS_CERT_FILE`, `HEPHAESTUS_TLS_KEY_FILE` | required in `file` mode, an error in `proxy` mode | Certificate chain and key files, written and renewed by another process |
| `HEPHAESTUS_PROXY_ADDRESS` | required in `proxy` mode, an error in `file` mode | `IP:port` hephaestus listens on for the reverse proxy |
| `HEPHAESTUS_PROXY_TRUSTED` | required in `proxy` mode, an error in `file` mode | Comma-separated CIDR prefixes of the proxy's own addresses; a prefix that contains the provisioning subnet is refused |
| `HEPHAESTUS_ADMIN_ADDRESS` | `127.0.0.1:9090` | Admin listener; a specific management IP, never a wildcard or an address inside the provisioning subnet |
| `HEPHAESTUS_DATA_DIR` | `/var/lib/hephaestus` | Absolute path for state |
| `HEPHAESTUS_DATABASE_URL` | `sqlite:<data dir>/hephaestus.db` | `sqlite:/abs/path.db` or `postgres://...` |
| `HEPHAESTUS_SHUTDOWN_GRACE` | `60s` | Time allowed to drain in-flight requests on SIGINT or SIGTERM |
| `HEPHAESTUS_LOG_LEVEL` | `info` | `slog` level |
| `HEPHAESTUS_LOG_FORMAT` | `json` | `json` or `text` (the `--log-format` flag wins) |

```sh
export HEPHAESTUS_FQDN=boot.example.com
export HEPHAESTUS_PROVISIONING_INTERFACE=vlan20
export HEPHAESTUS_PROVISIONING_ADDRESS=10.10.20.5
export HEPHAESTUS_PROVISIONING_SUBNET=10.10.20.0/24
export HEPHAESTUS_TLS_CERT_FILE=/etc/hephaestus/certs/fullchain.pem
export HEPHAESTUS_TLS_KEY_FILE=/etc/hephaestus/certs/privkey.pem
./bin/hephaestus serve
```

Ports 67, 69, 80 and 443 (proxyDHCP, TFTP, HTTP, HTTPS) are privileged; proxyDHCP also listens on 4011. The container image grants the binary `cap_net_bind_service`, which covers them, so it runs as UID 65532; a bare binary needs the same capability or root.

## TLS

hephaestus never obtains a certificate. The iPXE binaries chain to `https://<HEPHAESTUS_FQDN>/boot` and trust only the ISRG roots, so the certificate must chain to them (Let's Encrypt's default chain does). `HEPHAESTUS_TLS_MODE` says who holds it.

**`file` (default).** hephaestus serves a chain and key that another process writes and renews: certbot, lego, cert-manager. The certificate file must hold the full chain (`fullchain.pem`, not `cert.pem`) and the chain must reach one of the roots the embedded iPXE trusts (the four ISRG roots). It reads both files at startup and refuses to start when either is unreadable, the key does not match, the leaf does not cover `HEPHAESTUS_FQDN`, the leaf has expired, or the chain does not reach one of those roots, so a chain that does not reach those roots never reports ready. Once a minute it looks at both files, following symlinks, so a Kubernetes secret update counts. When one changed it reloads the pair under the same checks; a bad replacement, an untrusted chain included, is logged as a warning and the current pair keeps serving. The admin listener reports `tls:certificate` (critical; fails after the leaf expires) and `tls:expiry` (warns under 14 days). Mount the directory into the image read-only, readable by UID 65532:

```sh
docker run ... -v /etc/hephaestus/certs:/certs:ro \
  -e HEPHAESTUS_TLS_CERT_FILE=/certs/fullchain.pem -e HEPHAESTUS_TLS_KEY_FILE=/certs/privkey.pem ...
```

**`proxy`.** A reverse proxy (Caddy, Traefik, Envoy) terminates TLS 1.3 with a certificate that chains to the ISRG roots, and forwards plain HTTP to `HEPHAESTUS_PROXY_ADDRESS`. hephaestus then serves `/boot`, `/discover` and `/tftp` there and binds neither the HTTPS port nor any certificate. The proxy must replace or append to a client-supplied `X-Forwarded-For`, never pass it through, because hephaestus believes the header from a trusted peer and a passed-through one lets the client name any address. Caddy and Traefik do this by default. Envoy does not: its default (`use_remote_address: false`) leaves the header untouched, so set `use_remote_address: true` with `xff_num_trusted_hops: 0`. The proxy's own address must be in `HEPHAESTUS_PROXY_TRUSTED`. hephaestus reads the client address from the rightmost `X-Forwarded-For` entry that is not a trusted proxy, and answers 403 to any peer outside the trusted list and to a request with no usable header. Point `HEPHAESTUS_FQDN`'s DNS record at the proxy. Plain HTTP `:80` (`/start`, `/boot` as a chain to HTTPS, `/tftp`) stays on hephaestus, because the embedded chain script targets the next-server address, not the name.

The proxy must also disable TLS session tickets: iPXE 9261ebe hashes a NewSessionTicket that arrives before its own Finished into the handshake transcript, so a proxy that sends one early (Go's TLS stack does) fails the handshake. Caddy's Caddyfile has no option for this, so the example is Caddy's JSON config (`apps.tls.session_tickets.disabled`); a Caddyfile user converts theirs with `caddy adapt` and then adds that key. Traefik and Envoy were not tested; check theirs.

```json
{
	"apps": {
		"http": {"servers": {"boot": {
			"listen": [":443"],
			"tls_connection_policies": [{"protocol_min": "tls1.3"}],
			"routes": [{
				"match": [{"host": ["boot.example.com"]}],
				"handle": [{"handler": "reverse_proxy", "upstreams": [{"dial": "127.0.0.1:8443"}]}]
			}]
		}}},
		"tls": {"session_tickets": {"disabled": true}}
	}
}
```

`127.0.0.1:8443` is `HEPHAESTUS_PROXY_ADDRESS`; the proxy's address is in `HEPHAESTUS_PROXY_TRUSTED`.

## Run the image

The image's default state directory is `/var/lib/hephaestus`, owned by UID 65532. Mount persistent storage there: the SQLite database holds the fleet. A bind mount must be owned by 65532. Pass the variables from the env table above.

```sh
docker run --rm --network host --stop-timeout 60 -v hephaestus-data:/var/lib/hephaestus -v /etc/hephaestus/certs:/certs:ro --env-file hephaestus.env hephaestus:dev
```

What the host must provide:

- `CAP_NET_BIND_SERVICE` kept. The image fails under `--cap-drop ALL` unless you also pass `--cap-add NET_BIND_SERVICE`.
- Host networking, on the provisioning VLAN interface. proxyDHCP binds that interface (UDP 67 and 4011); TFTP, HTTP and, in `file` mode, HTTPS bind its primary address, `HEPHAESTUS_PROVISIONING_ADDRESS`.
- A LAN DNS record for `HEPHAESTUS_FQDN`: that address in `file` mode, the proxy's in `proxy` mode.
- Port 80 on that address free, and port 443 in `file` mode: the host's own web UI must not bind 0.0.0.0:80 or 0.0.0.0:443.
- A stop timeout of at least `HEPHAESTUS_SHUTDOWN_GRACE` (default 60s): `docker stop` waits only 10s unless you set `--stop-timeout` (compose: `stop_grace_period`). The boot servers and the admin server drain against one shared grace deadline.

## Prerequisites

- Go 1.26+
- [Task](https://taskfile.dev)
- Docker, through colima, for the Postgres tests and the iPXE builds
- hadolint

## Architecture

```
 provisioning VLAN (HEPHAESTUS_PROVISIONING_ADDRESS)       management address
 +------------------------------------------------+        +----------------+
 | proxyDHCP  ->  TFTP  ->  HTTP :80  ->  HTTPS :443        | admin :9090    |
 +------------------------------------------------+        +----------------+
                         |                                        |
                         +---------------+------------------------+
                                         |
                              SQLite or Postgres
 TLS: certificate files written by an external tool, or a TLS-terminating reverse proxy
```

Machines PXE-boot, chain into iPXE over TFTP, then fetch their boot script over HTTP and HTTPS. The admin listener stays off the VLAN.

## Utility catalog

The utility operating systems a machine can boot into (memtest86+, SystemRescue, Clonezilla, GParted, ShredOS, grml, the UEFI shell) are described in `internal/catalog/catalog.go`. Their sizes, hashes, extracted paths and pin ids are generated into `internal/catalog/catalog_pins.go` by:

```sh
task catalog:pin                      # pin every entry
task catalog:pin -- -only grml        # re-pin one entry; every other entry keeps its pin or absent reason
```

`task catalog:pin` runs inside a pinned container (`task catalog:image` builds it: the `catalogpin` tool plus gpg), so the host needs no gpg. It needs `DOCKER_HOST` set for colima, the internet and about 6.5 GB of downloads. For each object it downloads it over TLS 1.3 (every redirect hop is checked), checks the size, extracts the files the catalog boots, and records their hashes. A signed entry is verified against the key in `internal/catalog/keys/` before anything is parsed; a trust-on-first-use entry is pinned on first use. The report, with every hop, hash, signer, the ShredOS EFI-stub finding and whether each recipe's operating system honours BOOTIF, is `build/catalogpin/report.txt`.

An entry that cannot be pinned is not an error. It goes in the `absent` ledger of `catalog_pins.go` with a reason that starts with its class:

| Class | Meaning |
|---|---|
| `tls13:` | a hop did not complete TLS 1.3 |
| `network:` | DNS, connect, reset, timeout, another transfer failure, a local gpg failure such as no gpg, or a broken/missing committed key (up to 3 attempts for a transfer) |
| `http-<status>:` | an unexpected HTTP status (`5xx` and `429` get up to 3 attempts) |
| `integrity:` | a size, hash or extraction failure |
| `signature:` | signature verification, key fingerprint or provenance failure |
| `checksum:` | the upstream checksum list does not name the object's digest |

`internal/catalog/keys/PROVENANCE.md` records where each signer key came from and who corroborates it. **The grml key expires on 2028-04-21**: a re-pin after that date needs a refreshed `grml.asc` first.

## Admin CLI

The same binary is the admin CLI. Run it inside the container, where it reads the same `HEPHAESTUS_` environment as `serve` and writes the same database; it never opens the content store, so `serve` stays the only process that fetches and serves content.

```sh
docker exec hephaestus /hephaestus boot-next set 52:54:00:12:34:56 systemrescue
```

| Command | Effect |
|---|---|
| `boot-next set <mac\|uuid> <entry>` | Boots the machine into the utility on its next PXE boot, once. The machine must have booted through hephaestus before. A UUID that several machines share is refused with the list of their MACs; name one by MAC. An entry with no build for the machine's architecture is refused. It asks `serve` to fetch the utility, prints when the flag expires and the utility's state (`wanted`, `fetching`, `ready`, `failed`, `broken` or `stale_pin`), re-arms a failed or broken one and shows its previous error, and warns first for an entry that erases disks (ShredOS) |
| `boot-next clear <mac\|uuid>` | Cancels the machine's boot-next. It exits 0 with a note when there is none, or when the flag changed while the command ran |
| `boot-next list` | Every flag: machine, MACs, entry, architecture, when it was set, its age in seconds and its status: `expired`, `unknown_entry`, `unsupported`, or the utility's state (`not_requested` when nothing has asked for it) |
| `utility fetch <entry> [--arch amd64\|arm64]` | Asks `serve` to fetch the entry ahead of any boot, for every architecture it is built for or the one named; re-arms a failed or broken fetch |
| `utility list` | Every catalog entry by architecture: version, platforms, fetch state (`stale_pin` when the stored row is for an older pin), progress, last error, last change, whether its content is trust-on-first-use, and the minimum RAM in MiB |

Every command takes `--json`. A boot-next flag expires 24 hours after it was set: a machine that boots later boots as usual, and the flag is cleared when it does.

Shell access to the container is the authorization; the CLI has no network surface and no credentials of its own. It refuses to run, with exit status 1, while the database has pending migrations: start `hephaestus serve` once to apply them. A wrong number of arguments or an unknown flag exits with status 2.

## Operations: utility content

`serve` downloads the utility operating systems itself and keeps them in the data directory (`HEPHAESTUS_DATA_DIR`). The internet is needed only for the first fetch of each entry: once an entry is `ready`, it boots with no upstream.

| Path | Holds |
|---|---|
| `<data>/store` | The content store, an OCI image layout. Each entry is one tag, `utility/<entry>-<arch>:<pin id>`, over one blob per extracted file. Blob files are read-only (0444) and never edited in place |
| `<data>/spool` | Partial downloads, `<pin id>-<object index>` with an `.etag` sidecar. A fetch resumes from them after a restart. Created 0700 at startup when missing |
| `<data>/store.corrupt-<UTC time>` | A store `serve` set aside at startup (below). Only the newest is kept |

**A fetch needs free space** on the spool volume: twice the object's size plus a 1 GiB reserve. The data directory also holds the database, so `/health` warns on `disk:data_dir` below 10% free.

**When the store is moved aside.** If `index.json` cannot be decoded or has the wrong type, if the `oci-layout` version is unsupported, if a digest is malformed, or if a manifest does not match its digest or is truncated, `serve` renames `<data>/store` to `store.corrupt-<UTC time>`, logs one `store was unusable; moved aside` line at Error with the new path, starts an empty store and refetches. Any other failure, a permission or I/O error included, stops `serve` with that error and moves nothing. An empty `index.json` is not corruption: the store resets it in place and keeps its blobs.

**Startup reconciliation** runs after the database and the store are open and before anything serves or fetches. It logs one `utility reconciled` line with these counts:

| Count | What it did |
|---|---|
| `interrupted` | Rows that were `fetching` when the last process stopped go back to `wanted` |
| `deleted` | Rows of an entry or architecture the catalog no longer has are removed |
| `repinned` | A row at an older pin moves to `wanted` at the catalog's pin, with its attempts and error reset |
| `requeued` | A `ready` row whose tag or any layer is missing or the wrong size goes back to `wanted`, so a restored or repaired host refetches its cache unattended |
| `restored` | A `wanted` or `failed` row whose content is complete in the store becomes `ready` (a crash between tagging and the state update). A `broken` row stays broken |
| `spool_removed` | Spool files that no current pin owns are deleted |

**Garbage collection is restart-only.** After reconciliation, `serve` untags every `utility/*` tag that is not a current entry's, logs `utility garbage collected` with the `untagged` count, and deletes the blobs no tag reaches. The store spares unreferenced blobs written in the last hour. Tags outside `utility/` are never touched. Nothing collects while `serve` runs, so a catalog change frees its disk on the next restart. A collection that fails logs `utility garbage collection failed` at Warn and `serve` carries on: the garbage is only disk. A reconciliation that fails stops `serve` with the error, and so does content the store cannot read (a permission or I/O error): only content the store lacks (an unknown tag, a missing or wrong-sized layer) requeues a `ready` row.

**Fetch log lines.** Every state change of a cache row logs one Info line, `utility fetch`, with `entry`, `arch`, `pin`, `from`, `to`, `attempts`, `class` (`integrity` or `other` for a failure), `error`, `bytes` and `duration_ms`. An error that the fetcher could not store on a row logs `utility fetch loop error` at Warn and polling continues; a failed fetch never stops `serve`. A transient failure is retried after a backoff that starts at one minute, doubles and caps at one hour, and the tenth failure makes the row `broken`. An integrity failure (a hash that does not match, a redirect off https) makes it `broken` at once. `hephaestus utility fetch <entry>` re-arms a `failed` or `broken` row.

**`/health` items** (`GET /health` on the admin listener):

| Item | Meaning |
|---|---|
| `store:readable` | Critical. Passes when `index.json` decodes and the blob directory lists; a failure takes `/readyz` to 503 |
| `store:bytes` | Informational. The total size of the blob files, as an observed value in bytes |

**Metrics** (`GET /metrics` on the admin listener):

| Metric | Meaning |
|---|---|
| `hephaestus_utility_fetch_bytes_total` | Bytes downloaded from upstreams |
| `hephaestus_utility_fetch_failures_total{class}` | Failed fetches, by class `integrity` or `other` |
| `hephaestus_utility_boots_total{entry}` | Utility boot scripts served |

## Build

```sh
task ci            # fmt and tidy checks, lint, vuln, hadolint, go test -race, cross-compile,
                   # Postgres integration tests, coverage gate (needs DOCKER_HOST)
task ipxe:build    # rebuild the embedded iPXE binaries (needs DOCKER_HOST)
task image:build   # build hephaestus:dev for linux/arm64; PLATFORM=linux/amd64 overrides (needs DOCKER_HOST)
task e2e           # boot QEMU VMs through hephaestus in a Linux Docker daemon; about 2.5 minutes, not part of ci (colima needs DOCKER_HOST; see test/e2e/README.md)
```

With colima, set `DOCKER_HOST=unix:///Users/<you>/.colima/default/docker.sock`. The Dockerfile builds linux/amd64 and linux/arm64 by cross-compiling on the build platform; the final image is distroless and runs as `65532:65532`.

## Licences

The image contains code under copyleft licences:

- Modified GPLv2 iPXE binaries, built from iPXE commit `9261ebe` with the build config in `build/ipxe/`.
- Modified MPL-2.0 files, vendored under `internal/third_party/omniprovider/`.

Publishing an image therefore needs a public source repository or an accompanying source offer.

## M1 limits

M1 has no UI and performs no installs: a machine that is assigned but not ready holds at boot-decision row 8 and re-checks every 30 seconds.
