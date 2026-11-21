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
| `HEPHAESTUS_DATA_KEY`, `HEPHAESTUS_DATA_KEY_FILE` | exactly one required | Base64 of the 32-byte key that seals every stored secret, or a file holding it; see [Data Key](#data-key) |
| `HEPHAESTUS_OP_TOKEN`, `HEPHAESTUS_OP_TOKEN_FILE` | optional, at most one | 1Password service account token, or a file holding it; see [1Password Token](#1password-token) |
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
- Exactly one of `HEPHAESTUS_DATA_KEY` and `HEPHAESTUS_DATA_KEY_FILE` must be set, and it must decode to 32 bytes. At most one of `HEPHAESTUS_OP_TOKEN` and `HEPHAESTUS_OP_TOKEN_FILE` may be set. A secret file's trailing newline is dropped and an empty file is an error. Errors name the variable and never the value.
- `HEPHAESTUS_FETCH_WORKERS` must be a whole number from 1 to 16. It sizes one pool for both utility and Talos downloads.
- `HEPHAESTUS_LOG_FORMAT` must be `json` or `text`.

A minimal `file` mode environment:

```sh
export HEPHAESTUS_FQDN=boot.example.com
export HEPHAESTUS_PROVISIONING_INTERFACE=vlan20
export HEPHAESTUS_PROVISIONING_ADDRESS=10.10.20.5
export HEPHAESTUS_PROVISIONING_SUBNET=10.10.20.0/24
export HEPHAESTUS_TLS_CERT_FILE=/etc/hephaestus/certs/fullchain.pem
export HEPHAESTUS_TLS_KEY_FILE=/etc/hephaestus/certs/privkey.pem
export HEPHAESTUS_DATA_KEY_FILE=/etc/hephaestus/data-key
./bin/hephaestus serve
```

## Data Key

The data key seals every secret hephaestus stores: the resolved 1Password values, the git credential and the stored error texts. Generate one and keep a copy outside the host, for example in 1Password:

```sh
openssl rand -base64 32 > /etc/hephaestus/data-key
```

Prefer `HEPHAESTUS_DATA_KEY_FILE` over `HEPHAESTUS_DATA_KEY`, so the key is not in the process environment; the plain variable is for development. The admin CLI reads the same variables as `serve`, so a container started with `--env-file` and the key file mounted serves both.

At the first start `serve` stores a sealed canary, and at every start it opens it. A key that does not open it stops `serve` with an error that names the variable, so a wrong key is told apart from a damaged row. Any other stored value that fails to open is reported as a render error for its cluster, not a stop.

A lost key has no recovery in this release: the sealed values cannot be read, so restoring the key is the only remedy. Rotation is not built.

**Upgrading from the utility-only release (M2).** `serve` now refuses to start without a data key. Create one and set it before the first start of the new image; the database migration runs then.

## 1Password Token

`HEPHAESTUS_OP_TOKEN_FILE` (or `HEPHAESTUS_OP_TOKEN`) holds a 1Password service account token. It is optional: without it `serve` still starts and keeps serving the configs that are already approved, but a new tree, a refresh and a first credential are refused with an error that names the variable, and the `secrets:token` check warns while any cluster exists. A due refresh is skipped and stays pending until a token is set. The token is the one stored secret, besides the data key, that the deployment supplies.

Only `ref+op://VAULT/ITEM/[SECTION/]FIELD` references work, with no `?` query and no `#` fragment, and only as a whole string. See [Install Talos](first-install.md#what-the-definition-must-satisfy).

## Outbound Connections

`serve` opens outbound connections to the git hosts of the imported clusters, to `factory.talos.dev` and `assets.factory.talos.dev` for Talos boot files, to 1Password when a token is set, and to the utility mirrors. Every one requires TLS 1.3. The process-wide HTTP transport is set to a 10 s dial timeout, a 10 s handshake timeout and a 60 s response-header timeout. The admin CLI never opens a network connection.

## Exit Codes

`hephaestus` exits 0 on success, 1 on failure and 2 on a usage error (an unknown command or flag, or a stray argument).

[← Back to README](../README.md)
