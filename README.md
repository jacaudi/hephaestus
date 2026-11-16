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

**`file` (default).** hephaestus serves a chain and key that another process writes and renews: certbot, lego, cert-manager. The certificate file must hold the full chain (`fullchain.pem`, not `cert.pem`) and the chain must reach one of the roots the embedded iPXE trusts (the four ISRG roots). It reads both files at startup and refuses to start when either is unreadable, the key does not match, the leaf does not cover `HEPHAESTUS_FQDN`, the leaf has expired, or the chain does not reach one of those roots, so a certificate iPXE would reject never reports ready. Once a minute it looks at both files, following symlinks, so a Kubernetes secret update counts. When one changed it reloads the pair under the same checks; a bad replacement, an untrusted chain included, is logged as a warning and the current pair keeps serving. The admin listener reports `tls:certificate` (critical; fails after the leaf expires) and `tls:expiry` (warns under 14 days). Mount the directory into the image read-only, readable by UID 65532:

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

## Build

```sh
task ci            # fmt and tidy checks, lint, vuln, hadolint, go test -race, cross-compile,
                   # Postgres integration tests, coverage gate (needs DOCKER_HOST)
task ipxe:build    # rebuild the embedded iPXE binaries (needs DOCKER_HOST)
task image:build   # build hephaestus:dev for linux/arm64; PLATFORM=linux/amd64 overrides (needs DOCKER_HOST)
```

With colima, set `DOCKER_HOST=unix:///Users/<you>/.colima/default/docker.sock`. The Dockerfile builds linux/amd64 and linux/arm64 by cross-compiling on the build platform; the final image is distroless and runs as `65532:65532`.

## Licences

The image contains code under copyleft licences:

- Modified GPLv2 iPXE binaries, built from iPXE commit `9261ebe` with the build config in `build/ipxe/`.
- Modified MPL-2.0 files, vendored under `internal/third_party/omniprovider/`.

Publishing an image therefore needs a public source repository or an accompanying source offer.

## M1 limits

M1 has no UI and performs no installs: a machine that is assigned but not ready holds at boot-decision row 8 and re-checks every 30 seconds.
