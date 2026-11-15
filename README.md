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
| `HEPHAESTUS_PROVISIONING_INTERFACE` | required | Interface proxyDHCP listens on |
| `HEPHAESTUS_PROVISIONING_ADDRESS` | required | This host's IPv4 address on the provisioning subnet |
| `HEPHAESTUS_PROVISIONING_SUBNET` | required | Provisioning subnet, as an IPv4 CIDR prefix |
| `HEPHAESTUS_DATA_KEY` or `HEPHAESTUS_DATA_KEY_FILE` | required (set exactly one) | Base64 32-byte key that seals stored secrets |
| `HEPHAESTUS_CLOUDFLARE_TOKEN` or `HEPHAESTUS_CLOUDFLARE_TOKEN_FILE` | required (set exactly one) | Cloudflare token for the DNS-01 challenge |
| `HEPHAESTUS_ADMIN_ADDRESS` | `127.0.0.1:9090` | Admin listener; a specific management IP, never a wildcard or an address inside the provisioning subnet |
| `HEPHAESTUS_DATA_DIR` | `/var/lib/hephaestus` | Absolute path for state |
| `HEPHAESTUS_DATABASE_URL` | `sqlite:<data dir>/hephaestus.db` | `sqlite:/abs/path.db` or `postgres://...` |
| `HEPHAESTUS_ACME_DIRECTORY_URL` | `https://acme-v02.api.letsencrypt.org/directory` | ACME directory (https) |
| `HEPHAESTUS_SHUTDOWN_GRACE` | `60s` | Time allowed to drain in-flight requests on SIGINT or SIGTERM |
| `HEPHAESTUS_LOG_LEVEL` | `info` | `slog` level |
| `HEPHAESTUS_LOG_FORMAT` | `json` | `json` or `text` (the `--log-format` flag wins) |

Generate a data key, then run it. The key encrypts what hephaestus stores, so keep a copy: losing it makes the stored secrets unreadable.

```sh
export HEPHAESTUS_DATA_KEY="$(openssl rand -base64 32)"
export HEPHAESTUS_CLOUDFLARE_TOKEN=...
export HEPHAESTUS_FQDN=boot.example.com
export HEPHAESTUS_PROVISIONING_INTERFACE=vlan20
export HEPHAESTUS_PROVISIONING_ADDRESS=10.10.20.5
export HEPHAESTUS_PROVISIONING_SUBNET=10.10.20.0/24
./bin/hephaestus serve
```

Ports 67, 69, 80, 443 and 4011 (proxyDHCP, TFTP, HTTP, HTTPS) are privileged. The container image grants the binary `cap_net_bind_service`, which covers them, so it runs as UID 65532; a bare binary needs the same capability or root.

## Run the image

The image's default state directory is `/var/lib/hephaestus`, owned by UID 65532. Mount persistent storage there: the SQLite database holds the sealed ACME account and certificate, so losing it forces a re-issue. A bind mount must be owned by 65532. Pass the variables from the env table above. proxyDHCP listens on a host interface, so the container uses the host network.

```sh
docker run --rm --network host -v hephaestus-data:/var/lib/hephaestus --env-file hephaestus.env hephaestus:dev
```

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
 TLS: Let's Encrypt certificate for HEPHAESTUS_FQDN via Cloudflare DNS-01
```

Machines PXE-boot, chain into iPXE over TFTP, then fetch their boot script over HTTP and HTTPS. The admin listener stays off the VLAN.

## Build

```sh
task ci            # fmt and tidy checks, lint, vuln, hadolint, go test -race, cross-compile,
                   # Postgres integration tests, coverage gate (needs DOCKER_HOST)
task ipxe:build    # rebuild the embedded iPXE binaries (needs DOCKER_HOST)
task image:build   # build hephaestus:dev for linux/arm64 (needs DOCKER_HOST)
```

With colima, set `DOCKER_HOST=unix:///Users/<you>/.colima/default/docker.sock`. The Dockerfile builds linux/amd64 and linux/arm64 by cross-compiling on the build platform; the final image is distroless and runs as `65532:65532`.

## Licences

The image contains code under copyleft licences:

- Modified GPLv2 iPXE binaries, built from iPXE commit `9261ebe` with the build config in `build/ipxe/`.
- Modified MPL-2.0 files, vendored under `internal/third_party/omniprovider/`.

Publishing an image therefore needs a public source repository or an accompanying source offer.

## M1 limits

M1 has no UI and performs no installs: a machine that is assigned but not ready holds at boot-decision row 8 and re-checks every 30 seconds.
