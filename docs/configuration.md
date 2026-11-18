# Configuration

This page is the reference for how `hephaestus serve` is configured: every `HEPHAESTUS_*` environment variable, its default, and how it is validated.

`hephaestus serve` reads its configuration from the environment and refuses to start if any value is missing or invalid; every problem is reported together. The one command-line flag is `--log-format`. Precedence is the flag, then the environment, then the default.

| Variable | Default | Meaning |
|---|---|---|
| `HEPHAESTUS_FQDN` | required | Name the boot HTTPS certificate covers |
| `HEPHAESTUS_PROVISIONING_INTERFACE` | required | Interface proxyDHCP listens on; it must exist and hold the provisioning address |
| `HEPHAESTUS_PROVISIONING_ADDRESS` | required | This host's IPv4 address on the provisioning subnet |
| `HEPHAESTUS_PROVISIONING_SUBNET` | required | Provisioning subnet, as an IPv4 CIDR prefix |
| `HEPHAESTUS_TLS_MODE` | `file` | `file` or `proxy`; see [TLS](tls.md) |
| `HEPHAESTUS_TLS_CERT_FILE`, `HEPHAESTUS_TLS_KEY_FILE` | required in `file` mode, an error in `proxy` mode | Certificate chain and key files, written and renewed by another process |
| `HEPHAESTUS_PROXY_ADDRESS` | required in `proxy` mode, an error in `file` mode | `IP:port` hephaestus listens on for the reverse proxy |
| `HEPHAESTUS_PROXY_TRUSTED` | required in `proxy` mode, an error in `file` mode | Comma-separated CIDR prefixes of the proxy's own addresses; a prefix that contains the provisioning subnet is refused |
| `HEPHAESTUS_ADMIN_ADDRESS` | `127.0.0.1:9090` | Admin listener; a specific management IP, never a wildcard or an address inside the provisioning subnet |
| `HEPHAESTUS_DATA_DIR` | `/var/lib/hephaestus` | Absolute path for state |
| `HEPHAESTUS_DATABASE_URL` | `sqlite:<data dir>/hephaestus.db` | `sqlite:/abs/path.db` or `postgres://...` |
| `HEPHAESTUS_SHUTDOWN_GRACE` | `60s` | Time allowed to drain in-flight requests on SIGINT or SIGTERM; must be positive |
| `HEPHAESTUS_FETCH_WORKERS` | `4` | How many utility downloads run at once (1 to 16); see [Utility content](utility-content.md#parallel-downloads) |
| `HEPHAESTUS_LOG_LEVEL` | `info` | `slog` level |
| `HEPHAESTUS_LOG_FORMAT` | `json` | `json` or `text` (the `--log-format` flag wins) |

## Validation

- `HEPHAESTUS_FQDN` is lowercased and must be a fully qualified DNS name: at least two labels, 253 bytes at most.
- `HEPHAESTUS_PROVISIONING_ADDRESS` must be an IPv4 address inside `HEPHAESTUS_PROVISIONING_SUBNET`, and the interface named by `HEPHAESTUS_PROVISIONING_INTERFACE` must exist and have that address assigned.
- `HEPHAESTUS_ADMIN_ADDRESS` must be an IP and a port from 1 to 65535. A wildcard address, or one inside the provisioning subnet, is refused: the admin listener stays off the VLAN.
- `HEPHAESTUS_DATA_DIR` must be an absolute path.
- A TLS variable that belongs to the other mode is an error, not ignored: a leftover `HEPHAESTUS_TLS_CERT_FILE` in `proxy` mode stops `serve`.
- `HEPHAESTUS_FETCH_WORKERS` must be a whole number from 1 to 16.
- `HEPHAESTUS_LOG_FORMAT` must be `json` or `text`.

A minimal `file` mode environment:

```sh
export HEPHAESTUS_FQDN=boot.example.com
export HEPHAESTUS_PROVISIONING_INTERFACE=vlan20
export HEPHAESTUS_PROVISIONING_ADDRESS=10.10.20.5
export HEPHAESTUS_PROVISIONING_SUBNET=10.10.20.0/24
export HEPHAESTUS_TLS_CERT_FILE=/etc/hephaestus/certs/fullchain.pem
export HEPHAESTUS_TLS_KEY_FILE=/etc/hephaestus/certs/privkey.pem
./bin/hephaestus serve
```

## Exit Codes

`hephaestus` exits 0 on success, 1 on failure and 2 on a usage error (an unknown command or flag, or a stray argument).

[← Back to README](../README.md)
