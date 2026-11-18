# hephaestus

hephaestus gets bare metal from power-on to a correctly configured Talos node;
once caches are warm, no boot or install step depends on the internet or any
cloud service, and no secret sits anywhere unencrypted. It is a PXE boot and
fleet service: it answers proxyDHCP and TFTP, serves iPXE boot scripts over
HTTP and HTTPS, records each machine that boots, and decides what each boot
does.

It is not a DHCP server: it runs next to the one you have and only adds the PXE
options. It does not install anything yet (see What it does).

## What it does

A machine PXE-boots, chains into iPXE over TFTP, then fetches its boot script
from hephaestus over HTTP and HTTPS. hephaestus records the machine and answers
with one action: hold, boot the local disk, or boot a utility operating system
(memtest86+, SystemRescue, Clonezilla, GParted, ShredOS, grml or the UEFI
shell) that you requested once from the admin CLI. It has no UI and performs no
installs yet: a machine that is assigned but not ready holds at boot-decision
row 8 and re-checks every 30 seconds.

## Features

- **proxyDHCP and TFTP** — PXE-boots machines next to your existing DHCP
  server, with iPXE binaries embedded for BIOS and for amd64 and arm64 UEFI.
- **TLS 1.3 boot chain** — iPXE trusts only the ISRG roots; hephaestus serves
  a certificate that another tool renews, or sits behind a TLS-terminating
  proxy.
- **Per-machine boot decisions** — an ordered rule table, evaluated on every
  boot, picks hold, local disk or utility.
- **One-time utility boots** — `boot-next` boots a machine into a pinned
  utility OS once; downloads are pinned and cached, then boot with no upstream.
- **Admin CLI and listener** — the same binary manages boot-next and the
  utility cache; `/readyz`, `/health` and `/metrics` are served off the VLAN.
- **SQLite or Postgres** — SQLite in the data directory by default.

## Quick start

hephaestus runs as a container on the host that holds the provisioning VLAN.
You build the image from this repository.

Prerequisites:

- Docker (colima on macOS, with `DOCKER_HOST` set) and [Task](https://taskfile.dev),
  to build the image.
- A host that allows host networking, with an interface on the provisioning
  VLAN whose primary IPv4 address is the provisioning address.
- An existing DHCP server on that VLAN; hephaestus is proxyDHCP only.
- A LAN DNS record for your FQDN pointing at the provisioning address.
- A certificate for that FQDN that chains to the ISRG roots (Let's Encrypt's
  default chain does), written as `fullchain.pem` and `privkey.pem` by certbot,
  lego or cert-manager, readable by UID 65532.
- Ports 67, 69, 80, 443 and 4011 free on that host.

1. Build the image. It is `hephaestus:dev`, for linux/arm64 unless you pass
   a platform.

   ```sh
   task image:build PLATFORM=linux/amd64
   ```

2. Write `hephaestus.env`. In the default `file` TLS mode these six settings
   are not optional; the rest have defaults (see
   [Configuration](docs/configuration.md)).

   ```sh
   HEPHAESTUS_FQDN=boot.example.com
   HEPHAESTUS_PROVISIONING_INTERFACE=vlan20
   HEPHAESTUS_PROVISIONING_ADDRESS=10.10.20.5
   HEPHAESTUS_PROVISIONING_SUBNET=10.10.20.0/24
   HEPHAESTUS_TLS_CERT_FILE=/certs/fullchain.pem
   HEPHAESTUS_TLS_KEY_FILE=/certs/privkey.pem
   ```

3. Run it. The state directory holds the database, so keep it on a volume.

   ```sh
   docker run -d --name hephaestus --network host --stop-timeout 60 \
     -v hephaestus-data:/var/lib/hephaestus -v /etc/hephaestus/certs:/certs:ro \
     --env-file hephaestus.env hephaestus:dev
   ```

4. Check that it is ready. The admin listener defaults to `127.0.0.1:9090`.

   ```sh
   curl -i http://127.0.0.1:9090/readyz
   ```

   It answers `200` with the body `ok`. `serve` refuses to start on a missing
   or invalid setting and reports every problem together; read
   `docker logs hephaestus` if it exits.

To run the binary without a container, build it with `task build`, export the
same variables and run `./bin/hephaestus serve`. It needs `CAP_NET_BIND_SERVICE`
or root. What the host must provide is in [Run the image](docs/deployment.md).

## How it works

```
 provisioning VLAN (HEPHAESTUS_PROVISIONING_ADDRESS)       management address
 +---------------------------------------------------+     +----------------+
 | proxyDHCP -> TFTP -> HTTP :80 -> HTTPS :443       |     | admin :9090    |
 +---------------------------------------------------+     +----------------+
                          |                                        |
                          +-------------------+--------------------+
                                              |
              SQLite or Postgres, and the content store in the data dir
 TLS: certificate files written by an external tool, or a TLS-terminating proxy
```

| Component | Job |
|---|---|
| proxyDHCP | Offers the PXE boot file on UDP 67 and 4011, on the provisioning interface |
| TFTP | Serves the embedded iPXE binaries |
| HTTP :80 | `/start`, `/boot` (as a chain to HTTPS), `/tftp` and `/u/` utility files |
| HTTPS :443 | `/boot` and `/discover` over TLS 1.3 (`file` mode; the proxy serves them in `proxy` mode) |
| Admin | `/healthz`, `/readyz`, `/health` and `/metrics`, on a management IP off the VLAN |
| Database and store | Fleet state in SQLite or Postgres; utility downloads in the data directory |

## Documentation

**Run it**

- [Run the image](docs/deployment.md) - what the host must provide, ports and `docker run`
- [Configuration](docs/configuration.md) - every `HEPHAESTUS_*` variable, default and rule
- [TLS](docs/tls.md) - file mode and proxy mode, including the session-ticket rule

**Operate it**

- [Admin CLI](docs/admin-cli.md) - `boot-next`, `utility` and `version`
- [Utility content](docs/utility-content.md) - the cache, parallel downloads, free space, recovery
- [Observability](docs/observability.md) - admin routes, `/health` items and metrics
- [Utility catalog](docs/utility-catalog.md) - the entries and how `task catalog:pin` pins them

**Develop and verify**

- [Development](docs/development.md) - prerequisites and `task` commands
- [End-to-end test](test/e2e/README.md) - QEMU boots through hephaestus with `task e2e`
- [M1 lab bring-up](docs/lab/m1-bring-up.md) - real-hardware checks for the boot path
- [M2 lab bring-up](docs/lab/m2-utility-boots.md) - one real boot per utility entry

**Background**

- [Bundled code licences](docs/licensing.md) - copyleft code in the image
- [iPXE TLS 1.3 note](docs/upstream/ipxe-tls13-new-session-ticket-transcript.md) - why session tickets are off

## Credits

- [Omni bare-metal infrastructure provider](https://github.com/siderolabs/omni-infra-provider-bare-metal) (v0.15.0, MPL-2.0): the vendored proxyDHCP and TFTP code.
- [iPXE](https://github.com/ipxe/ipxe) (commit `9261ebe`): the embedded boot binaries.
