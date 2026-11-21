# Run the Image

This is the task guide for running the hephaestus container image: what the host must provide, the ports it binds, and the `docker run` command.

The image is built with `task image:build` (see [Development](development.md)); it is distroless, runs as `65532:65532`, and its default command is `serve`. Its default state directory is `/var/lib/hephaestus`, owned by UID 65532. Mount persistent storage there: the SQLite database holds the fleet. A bind mount must be owned by 65532. Pass the variables from [Configuration](configuration.md), including the data key, which is required.

```sh
docker run --rm --network host --stop-timeout 60 -v hephaestus-data:/var/lib/hephaestus -v /etc/hephaestus/certs:/certs:ro -v /etc/hephaestus/data-key:/run/secrets/data-key:ro --env-file hephaestus.env hephaestus:dev
```

## Ports

Ports 67, 69, 80 and 443 (proxyDHCP, TFTP, HTTP, HTTPS) are privileged; proxyDHCP also listens on 4011. The container image grants the binary `cap_net_bind_service`, which covers them, so it runs as UID 65532; a bare binary needs the same capability or root. The admin listener is on `HEPHAESTUS_ADMIN_ADDRESS` (default `127.0.0.1:9090`).

## What the Host Must Provide

- `CAP_NET_BIND_SERVICE` kept. The image fails under `--cap-drop ALL` unless you also pass `--cap-add NET_BIND_SERVICE`.
- Host networking, on the provisioning VLAN interface. proxyDHCP binds that interface (UDP 67 and 4011); TFTP, HTTP and, in `file` mode, HTTPS bind its primary address, `HEPHAESTUS_PROVISIONING_ADDRESS`.
- A LAN DNS record for `HEPHAESTUS_FQDN`: that address in `file` mode, the proxy's in `proxy` mode. A Talos node fetches its config from `https://<HEPHAESTUS_FQDN>/config`, so the node must resolve the name and trust the certificate.
- A data key, mounted read-only and readable by UID 65532 (`HEPHAESTUS_DATA_KEY_FILE`), and a copy of it kept elsewhere. Without it `serve` does not start; a lost one has no recovery. See [Data Key](configuration.md#data-key).
- Outbound TLS 1.3 to the git hosts of your clusters, `factory.talos.dev` and `assets.factory.talos.dev`, and to 1Password if a token is set. Talos boot files are about 100 MB per schematic, version and architecture, so size the data volume for them and the utility cache together. Nothing on the boot path calls out once a machine's files are cached.
- Port 80 on that address free, and port 443 in `file` mode: the host's own web UI must not bind 0.0.0.0:80 or 0.0.0.0:443.
- A stop timeout of at least `HEPHAESTUS_SHUTDOWN_GRACE` (default 60s): `docker stop` waits only 10s unless you set `--stop-timeout` (compose: `stop_grace_period`). The boot servers and the admin server drain against one shared grace deadline. A git fetch, a 1Password resolve or a download still running at shutdown is abandoned and retried after the next start.

The data directory holds the database, the content store, the spool and the git object store (`<data>/git`). Everything secret in it is sealed under the data key, except what the imported repositories themselves contain.

For the certificate and the proxy, see [TLS](tls.md). To install Talos, see [Install Talos](first-install.md). For a first run on real hardware, follow the [M1 lab bring-up](lab/m1-bring-up.md).

[← Back to README](../README.md)
