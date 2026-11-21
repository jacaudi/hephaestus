# hephaestus

hephaestus gets bare metal from power-on to a correctly configured Talos node;
once caches are warm, no boot step depends on the internet or any cloud
service, and no secret sits anywhere unencrypted. It is a PXE boot and fleet
service: it answers proxyDHCP and TFTP, serves iPXE boot scripts over HTTP and
HTTPS, records each machine that boots, decides what each boot does, and
installs Talos from a Topf cluster definition in your own git repository.

It is not a DHCP server: it runs next to the one you have and only adds the PXE
options. Offline installs are not built yet: the Talos boot files still come
from the public image factory the first time (see What It Does).

> [!WARNING]
> **Agentically generated.** This codebase was produced through agentic,
> spec-driven development: each feature began as a written design and
> implementation spec, then a coding agent executed the plan under human
> review. Tests, code review, and CI gates apply as they would for any
> project, but the authorship pattern is not a single human contributor —
> keep that in mind when evaluating fit for your environment.

## What It Does

A machine PXE-boots, chains into iPXE over TFTP, then fetches its boot script
from hephaestus over HTTP and HTTPS. hephaestus records the machine and answers
with one action: hold, boot the local disk, boot a utility operating system
(memtest86+, SystemRescue, Clonezilla, GParted, ShredOS, grml or the UEFI
shell) that you requested once from the admin CLI, or install Talos.

For an install you import a Topf cluster definition from git, read the redacted
diff of what it renders and approve it, then approve a discovered machine into
one of its hosts. hephaestus fetches the Talos boot files from the image
factory, serves the install script, and hands the machine its config once, over
HTTPS, against a single-use ticket; you confirm the install. It has no UI: the
admin CLI does all of it. A machine that is assigned but whose files are not
cached holds and re-checks every 30 seconds.

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
- **Git-imported clusters** — hephaestus polls a Topf definition in an https
  git repository and holds each change as a pending pair until you approve it.
- **Secrets from 1Password** *(optional)* — `ref+op://` references resolve
  through a service account and are stored sealed under a data key; boots never
  call 1Password.
- **Talos installs** — an approved machine gets its install script and a
  single-use config ticket; the boot files are cached from the image factory.
- **Admin CLI and listener** — the same binary manages clusters, machines,
  boot-next and the utility cache, and reads the audit log; `/readyz`,
  `/health` and `/metrics` are served off the VLAN.
- **SQLite or Postgres** — SQLite in the data directory by default.

## Quick Start

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
- Outbound TLS 1.3 from that host to your git host and `factory.talos.dev`
  (and to 1Password, if a definition uses references), for installs.

1. Build the image. It is `hephaestus:dev`, for linux/arm64 unless you pass
   a platform.

   ```sh
   task image:build PLATFORM=linux/amd64
   ```

2. Create the data key, which seals the secrets hephaestus stores. Keep a copy
   somewhere else: a lost key has no recovery, and a wrong one stops `serve`.

   ```sh
   openssl rand -base64 32 > /etc/hephaestus/data-key
   chown 65532:65532 /etc/hephaestus/data-key && chmod 0400 /etc/hephaestus/data-key
   ```

3. Write `hephaestus.env`. In the default `file` TLS mode these seven settings
   are not optional; the rest have defaults (see
   [Configuration](docs/configuration.md)). A 1Password service account token
   is optional (`HEPHAESTUS_OP_TOKEN_FILE`).

   ```sh
   HEPHAESTUS_FQDN=boot.example.com
   HEPHAESTUS_PROVISIONING_INTERFACE=vlan20
   HEPHAESTUS_PROVISIONING_ADDRESS=10.10.20.5
   HEPHAESTUS_PROVISIONING_SUBNET=10.10.20.0/24
   HEPHAESTUS_TLS_CERT_FILE=/certs/fullchain.pem
   HEPHAESTUS_TLS_KEY_FILE=/certs/privkey.pem
   HEPHAESTUS_DATA_KEY_FILE=/run/secrets/data-key
   ```

4. Run it. The state directory holds the database, so keep it on a volume.

   ```sh
   docker run -d --name hephaestus --network host --stop-timeout 60 \
     -v hephaestus-data:/var/lib/hephaestus -v /etc/hephaestus/certs:/certs:ro \
     -v /etc/hephaestus/data-key:/run/secrets/data-key:ro \
     --env-file hephaestus.env hephaestus:dev
   ```

5. Check that it is ready. The admin listener defaults to `127.0.0.1:9090`.

   ```sh
   curl -i http://127.0.0.1:9090/readyz
   ```

   It answers `200` with the body `ok`. `serve` refuses to start on a missing
   or invalid setting and reports every problem together; read
   `docker logs hephaestus` if it exits.

To run the binary without a container, build it with `task build`, export the
same variables and run `./bin/hephaestus serve`. It needs `CAP_NET_BIND_SERVICE`
or root. What the host must provide is in [Run the image](docs/deployment.md).
To install Talos on a machine, follow [Install Talos](docs/first-install.md).

Upgrading from a release without clusters (M2)? `serve` now refuses to start
without a data key; add one before restarting.

## How It Works

![A booting machine on the provisioning VLAN reaches four hephaestus services in boot order (proxyDHCP, TFTP, HTTP :80 and HTTPS :443), and the HTTPS service gets its TLS from certificate files or a TLS-terminating proxy. The services and an admin listener off the VLAN use the data directory, where a fetcher fills the content store from upstream mirrors over TLS 1.3.](docs/images/architecture.svg)

| Component | Job |
|---|---|
| proxyDHCP | Offers the PXE boot file on UDP 67 and 4011, on the provisioning interface |
| TFTP | Serves the embedded iPXE binaries |
| HTTP :80 | `/start`, `/boot` (as a chain to HTTPS), `/tftp`, `/u/` utility files and `/t/` Talos boot files |
| HTTPS :443 | `/boot`, `/discover` and `/config` over TLS 1.3 (`file` mode; the proxy serves them in `proxy` mode) |
| Cluster poller | Fetches each imported git repository, resolves its 1Password references and renders pending pairs; never on the boot path |
| Admin | `/healthz`, `/readyz`, `/health` and `/metrics`, on a management IP off the VLAN |
| Database and store | Fleet, cluster and ticket state in SQLite or Postgres, secrets sealed; downloads and the git object store in the data directory |

## Documentation

**Run it**

- [Run the image](docs/deployment.md) - what the host must provide, ports and `docker run`
- [Configuration](docs/configuration.md) - every `HEPHAESTUS_*` variable, default and rule
- [TLS](docs/tls.md) - file mode and proxy mode, including the session-ticket rule

**Operate it**

- [Install Talos](docs/first-install.md) - import a cluster, approve a machine, reinstall, and the runbooks
- [Admin CLI](docs/admin-cli.md) - `cluster`, `machine`, `audit`, `unknown-action`, `boot-next`, `utility` and `version`
- [Utility content](docs/utility-content.md) - the cache, parallel downloads, free space, recovery
- [Observability](docs/observability.md) - admin routes, `/health` items, metrics and alerts
- [Utility catalog](docs/utility-catalog.md) - the entries and how `task catalog:pin` pins them

**Develop and verify**

- [Development](docs/development.md) - prerequisites and `task` commands
- [End-to-end test](test/e2e/README.md) - QEMU boots through hephaestus with `task e2e`
- [M1 lab bring-up](docs/lab/m1-bring-up.md) - real-hardware checks for the boot path
- [M2 lab bring-up](docs/lab/m2-utility-boots.md) - one real boot per utility entry

**Background**

- [Bundled code licences](docs/licensing.md) - copyleft and vendored code in the image
- [iPXE TLS 1.3 note](docs/upstream/ipxe-tls13-new-session-ticket-transcript.md) - why session tickets are off

## Credits

- [Omni bare-metal infrastructure provider](https://github.com/siderolabs/omni-infra-provider-bare-metal) (v0.15.0, MPL-2.0): the vendored proxyDHCP and TFTP code.
- [iPXE](https://github.com/ipxe/ipxe) (commit `9261ebe`): the embedded boot binaries.
- [Topf](https://github.com/postfinance/topf) (commit `70cdbc8`, MIT): the vendored cluster loader and per-node render.
- [Image Factory](https://github.com/siderolabs/image-factory) (v1.7.1, MPL-2.0): the vendored schematic model and ID computation.

## License

[MIT](LICENSE) © 2026 jacaudi
