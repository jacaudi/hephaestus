# hephaestus

hephaestus is a PXE boot and fleet service. It takes bare metal from power-on to
a correctly configured Talos node. It records each machine that boots, decides
what each boot does, and installs Talos from a Topf cluster definition in your
git repository. When the caches are warm, no boot step needs the internet or a
cloud service. hephaestus seals the secrets it stores.

hephaestus is not a DHCP server. It runs next to your DHCP server and adds only
the PXE options. Offline installs are not built yet: the first time, the Talos
boot files come from the public image factory.

> [!WARNING]
> **Agentically generated.** This codebase was produced through agentic,
> spec-driven development: each feature began as a written design and
> implementation spec, then a coding agent executed the plan under human
> review. Tests, code review, and CI gates apply as they would for any
> project, but the authorship pattern is not a single human contributor —
> keep that in mind when evaluating fit for your environment.

## What It Does

A machine PXE-boots and asks hephaestus for its boot script. hephaestus records
the machine and gives it one action: hold, boot the local disk, boot a utility
operating system, or install Talos. To install Talos, you import a Topf cluster
definition from git, approve what it renders, and approve a machine into one of
its hosts. hephaestus then serves the Talos boot files and gives the machine its
config once, with a single-use ticket.

## Features

- **proxyDHCP and TFTP** — PXE-boots machines next to your DHCP server. It
  embeds iPXE binaries for BIOS and for amd64 and arm64 UEFI.
- **TLS 1.3 boot chain** — iPXE trusts only the ISRG roots. hephaestus serves a
  certificate that another tool renews, or sits behind a TLS-terminating proxy.
- **Per-machine boot decisions** — an ordered rule table, checked on every boot,
  picks hold, local disk or utility.
- **One-time utility boots** — `boot-next` boots a machine into a utility OS
  once: memtest86+, SystemRescue, Clonezilla, GParted, ShredOS, grml or the UEFI
  shell. Pinned downloads are cached, so a boot needs no upstream.
- **Git-imported clusters** — polls a Topf definition in an https git
  repository. It holds each change as a pending pair until you approve it.
- **Secrets from 1Password** *(optional)* — `ref+op://` references resolve
  through a service account and are stored sealed. A boot never calls 1Password.
- **Talos installs** — an approved machine gets an install script and a
  single-use config ticket. A machine whose files are not cached holds and
  checks again every 30 seconds.
- **Admin CLI and listener** — one binary manages clusters, machines,
  `boot-next` and the utility cache, and reads the audit log. It has no UI.
  `/readyz`, `/health` and `/metrics` are served off the VLAN.
- **SQLite or Postgres** — SQLite in the data directory by default.

## Quick Start

hephaestus runs as a container on the host that is on the provisioning VLAN. No
image is published, so you build it from this repository.

Prerequisites:

- Docker and [Task](https://taskfile.dev), to build the image. On macOS, use
  colima and set `DOCKER_HOST`.
- A host that allows host networking, with an interface on the provisioning
  VLAN. The primary IPv4 address of that interface is the provisioning address.
- A DHCP server on that VLAN. hephaestus is proxyDHCP only.
- A LAN DNS record that points your FQDN to the provisioning address.
- A certificate for that FQDN that chains to the ISRG roots. The default chain
  of Let's Encrypt does. certbot, lego or cert-manager must write it as
  `fullchain.pem` and `privkey.pem`, readable by UID 65532.
- Free ports 67, 69, 80, 443 and 4011 on that host.
- For installs, outbound TLS 1.3 from that host to your git host,
  `factory.talos.dev` and `assets.factory.talos.dev`. If a definition uses
  1Password references, also to 1Password.

1. Build the image. It is named `hephaestus:dev`. The platform is linux/arm64
   unless you set `PLATFORM`.

   ```sh
   task image:build PLATFORM=linux/amd64
   ```

2. Create the data key. hephaestus uses it to seal the secrets it stores. Keep
   a copy in a different place. A lost key has no recovery, and a wrong key
   stops `serve`.

   ```sh
   openssl rand -base64 32 > /etc/hephaestus/data-key
   chown 65532:65532 /etc/hephaestus/data-key && chmod 0400 /etc/hephaestus/data-key
   ```

3. Write `hephaestus.env`. In the default `file` TLS mode, you must set these
   seven variables. The other variables have defaults (see
   [Configuration](docs/configuration.md)). `HEPHAESTUS_OP_TOKEN_FILE` is
   optional. It holds a 1Password service account token.

   ```sh
   HEPHAESTUS_FQDN=boot.example.com
   HEPHAESTUS_PROVISIONING_INTERFACE=vlan20
   HEPHAESTUS_PROVISIONING_ADDRESS=10.10.20.5
   HEPHAESTUS_PROVISIONING_SUBNET=10.10.20.0/24
   HEPHAESTUS_TLS_CERT_FILE=/certs/fullchain.pem
   HEPHAESTUS_TLS_KEY_FILE=/certs/privkey.pem
   HEPHAESTUS_DATA_KEY_FILE=/run/secrets/data-key
   ```

4. Start the container. The state directory holds the database, so keep it on a
   volume.

   ```sh
   docker run -d --name hephaestus --network host --stop-timeout 60 \
     -v hephaestus-data:/var/lib/hephaestus -v /etc/hephaestus/certs:/certs:ro \
     -v /etc/hephaestus/data-key:/run/secrets/data-key:ro \
     --env-file hephaestus.env hephaestus:dev
   ```

5. Make sure that it is ready. The admin listener is at `127.0.0.1:9090` by
   default.

   ```sh
   curl -i http://127.0.0.1:9090/readyz
   ```

   It answers `200` with the body `ok`. If a setting is missing or not valid,
   `serve` does not start and reports every problem together. If the container
   exits, read `docker logs hephaestus`.

To run the binary without a container, run `task build`, export the same
variables, and run `./bin/hephaestus serve`. It needs `CAP_NET_BIND_SERVICE` or
root. For the host requirements, see [Run the Image](docs/deployment.md). To
install Talos on a machine, see [Install Talos](docs/first-install.md).

If you upgrade from the utility-only release (M2), add a data key before you
restart. `serve` does not start without one.

## How It Works

![A booting machine on the provisioning VLAN reaches four hephaestus services in boot order (proxyDHCP, TFTP, HTTP :80 and HTTPS :443), and the HTTPS service gets its TLS from certificate files or a TLS-terminating proxy. The services and an admin listener off the VLAN use the data directory, where a fetcher fills the content store from upstream mirrors over TLS 1.3.](docs/images/architecture.svg)

| Component | Job |
|---|---|
| proxyDHCP | Offers the PXE boot file on UDP 67 and 4011, on the provisioning interface |
| TFTP | Serves the embedded iPXE binaries |
| HTTP :80 | Serves `/start`, `/boot` (a chain to HTTPS), `/tftp`, `/u/` utility files and `/t/` Talos boot files |
| HTTPS :443 | In `file` mode, serves `/boot`, `/discover` and `/config` over TLS 1.3. In `proxy` mode, a TLS proxy terminates TLS and hephaestus serves them on `HEPHAESTUS_PROXY_ADDRESS` |
| Cluster poller | Fetches each imported git repository, resolves its 1Password references and renders pending pairs. It is never on the boot path |
| Admin | Serves `/healthz`, `/readyz`, `/health` and `/metrics` on a management IP off the VLAN |
| Database and store | Keeps fleet, cluster and ticket state in SQLite or Postgres, with secrets sealed. Keeps downloads and the git object store in the data directory |

## Documentation

**Run it**

- [Run the Image](docs/deployment.md) - what the host must provide, ports and `docker run`
- [Configuration](docs/configuration.md) - every `HEPHAESTUS_*` variable, default and rule
- [TLS](docs/tls.md) - file mode and proxy mode, including the session-ticket rule

**Operate it**

- [Install Talos](docs/first-install.md) - import a cluster, approve a machine, reinstall, and the runbooks
- [Admin CLI](docs/admin-cli.md) - `cluster`, `machine`, `audit`, `unknown-action`, `boot-next`, `utility` and `version`
- [Utility Content](docs/utility-content.md) - the cache, parallel downloads, free space, recovery
- [Observability](docs/observability.md) - admin routes, `/health` items, metrics and alerts
- [Utility Catalog](docs/utility-catalog.md) - the entries and how `task catalog:pin` pins them

**Develop and verify**

- [Development](docs/development.md) - prerequisites and `task` commands
- [End-to-End Test](test/e2e/README.md) - QEMU boots through hephaestus with `task e2e`
- [M1 Lab Bring-Up](docs/lab/m1-bring-up.md) - real-hardware checks for the boot path
- [M2 Lab Bring-Up](docs/lab/m2-utility-boots.md) - one real boot per utility entry

**Background**

- [Bundled Code Licences](docs/licensing.md) - copyleft and vendored code in the image
- [iPXE TLS 1.3 Note](docs/upstream/ipxe-tls13-new-session-ticket-transcript.md) - why session tickets are off

**Project**

- [Project Status](docs/status.md) - milestones done, work left, open follow-ups and conventions
- [Issues](https://github.com/jacaudi/hephaestus/issues) - report a bug or ask for help

## Credits

- [Omni bare-metal infrastructure provider](https://github.com/siderolabs/omni-infra-provider-bare-metal) (v0.15.0, MPL-2.0): the vendored proxyDHCP and TFTP code.
- [iPXE](https://github.com/ipxe/ipxe) (commit `9261ebe`): the embedded boot binaries.
- [Topf](https://github.com/postfinance/topf) (commit `70cdbc8`, MIT): the vendored cluster loader and per-node render.
- [Image Factory](https://github.com/siderolabs/image-factory) (v1.7.1, MPL-2.0): the vendored schematic model and ID computation.

## License

[MIT](LICENSE) © 2026 jacaudi
