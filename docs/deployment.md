# Run the Image

This is the task guide for running the hephaestus container image: what the host must provide, the ports it binds, and the `docker run` command.

The image is built with `task image:build` (see [Development](development.md)); it is distroless, runs as `65532:65532`, and its default command is `serve`. Its default state directory is `/var/lib/hephaestus`, owned by UID 65532. Mount persistent storage there: the SQLite database holds the fleet. A bind mount must be owned by 65532. Pass the variables from [Configuration](configuration.md).

```sh
docker run --rm --network host --stop-timeout 60 -v hephaestus-data:/var/lib/hephaestus -v /etc/hephaestus/certs:/certs:ro --env-file hephaestus.env hephaestus:dev
```

## Ports

Ports 67, 69, 80 and 443 (proxyDHCP, TFTP, HTTP, HTTPS) are privileged; proxyDHCP also listens on 4011. The container image grants the binary `cap_net_bind_service`, which covers them, so it runs as UID 65532; a bare binary needs the same capability or root. The admin listener is on `HEPHAESTUS_ADMIN_ADDRESS` (default `127.0.0.1:9090`).

## What the Host Must Provide

- `CAP_NET_BIND_SERVICE` kept. The image fails under `--cap-drop ALL` unless you also pass `--cap-add NET_BIND_SERVICE`.
- Host networking, on the provisioning VLAN interface. proxyDHCP binds that interface (UDP 67 and 4011); TFTP, HTTP and, in `file` mode, HTTPS bind its primary address, `HEPHAESTUS_PROVISIONING_ADDRESS`.
- A LAN DNS record for `HEPHAESTUS_FQDN`: that address in `file` mode, the proxy's in `proxy` mode.
- Port 80 on that address free, and port 443 in `file` mode: the host's own web UI must not bind 0.0.0.0:80 or 0.0.0.0:443.
- A stop timeout of at least `HEPHAESTUS_SHUTDOWN_GRACE` (default 60s): `docker stop` waits only 10s unless you set `--stop-timeout` (compose: `stop_grace_period`). The boot servers and the admin server drain against one shared grace deadline.

For the certificate and the proxy, see [TLS](tls.md). For a first run on real hardware, follow the [M1 lab bring-up](lab/m1-bring-up.md).

[← Back to README](../README.md)
