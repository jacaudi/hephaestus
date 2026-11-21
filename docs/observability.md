# Observability

This page is the reference for what the admin listener reports: its routes, the `/health` items and the metrics.

The admin listener is on `HEPHAESTUS_ADMIN_ADDRESS` (default `127.0.0.1:9090`) and serves only `GET /healthz`, `GET /readyz`, `GET /health` and `GET /metrics`. `/healthz` fails only once the service has stopped; `/readyz` is 200 only when the service is ready and every critical check passes (add `?verbose` to list every check); `/health` is a health+json document that returns 503 when a critical check fails.

Other checks: `tls:certificate` and `tls:expiry` are described in [TLS](tls.md), and `/health` warns on `disk:data_dir` when the data directory's disk has less than 10% free. The cluster and install checks are in the table below.

**`/health` items** (`GET /health` on the admin listener):

| Item | Meaning |
|---|---|
| `store:readable` | Critical. Passes when `index.json` decodes and the blob directory lists; a failure takes `/readyz` to 503 |
| `store:bytes` | Informational. The total size of the blob files, as an observed value in bytes |
| `render:approved` | Critical. Fails while an approved pair cannot be rendered (a tree missing from the cache or the git store, a stored value that will not open, a render error), naming the cluster and host. `/config` answers 404 for it. It clears when a render of the cluster's current approved pair next succeeds, so approving a new pair clears it |
| `git:fetch` | Warns when a cluster's last fetch failed or is older than 30 minutes, or it has a failed tree |
| `secrets:cache` | Warns when a cluster's last refresh was incomplete, or it has had no successful refresh for 2 days |
| `secrets:token` | Warns while any cluster exists and no 1Password token is set |
| `secrets:budget` | Warns at 500 1Password requests in 24 hours, half of 1,000, the smallest account tier's daily limit |
| `secrets:client` | Warns after 3 consecutive whole-call 1Password failures. If the errors say "recovered by wazero", restart `serve`: the SDK's core cannot be rebuilt in the process. It clears on the next successful call. It never fails `/readyz` |
| `talos:cache` | Warns while any Talos row is `broken`, naming its schematic, version and architecture; `machine fetch` re-arms it |
| `config:pending` | Warns for each config ticket still live 30 minutes after it was minted: one nobody asked for points at the node's DNS, clock and trust of the boot certificate; one that was asked for points at a refusal (`audit list --machine`) or a delivery that was not recorded. See [Install Talos](first-install.md#when-something-goes-wrong) |

**Metrics** (`GET /metrics` on the admin listener):

| Metric | Meaning |
|---|---|
| `hephaestus_utility_fetch_bytes_total` | Bytes downloaded from upstreams |
| `hephaestus_utility_fetch_failures_total{class}` | Failed fetches, by class `integrity` or `other` |
| `hephaestus_utility_boots_total{entry}` | Utility boot scripts served |
| `hephaestus_talos_fetch_bytes_total` | Bytes of Talos boot files downloaded from the image factory |
| `hephaestus_talos_fetch_failures_total{class}` | Failed Talos fetches, by class `integrity` or `other` |
| `hephaestus_git_fetches_total{outcome}` | Git fetches of cluster branches, by outcome `ok` or `error` |
| `hephaestus_secrets_requests` | Gauge: requests to 1Password in the last 24 hours, counted in memory (it resets on restart) |
| `hephaestus_tickets_minted_total` | Config tickets minted for Talos installs |
| `hephaestus_config_requests_total{outcome}` | `/config` requests, by outcome: `served`, or `refused_<reason>` (for example `refused_unknown_ticket`, `refused_invalid_ticket`, `refused_stale_pair`, `refused_drift`, `refused_off_subnet`) |
| `hephaestus_config_record_failures_total` | Failed tries to record a delivered config |
| `hephaestus_alerts_total{kind}` | Alerts raised, by kind; see below |

For the fetch log lines, see [Utility content](utility-content.md#fetch-log-lines); a Talos row logs the same fields as `talos fetch`, with `schematic`, `version` and `arch` in place of the entry and pin.

## Alerts

An alert is an audit event and a count of `hephaestus_alerts_total{kind}`; there is no UI yet, so read them with `audit list`.

| Kind | Audit event | Raised when |
|---|---|---|
| `drift` | `cluster.drift` | The render of an approved pair no longer hashes to what was approved. The host holds until `cluster approve <name> --host <host> --hash <hash>` |
| `refresh_incomplete` | `cluster.refresh_incomplete` | A refresh could not resolve every reference; the approved values stay and no pair is created from partial data |
| `talos_broken` | `talos.broken` | A Talos row went `broken`: an integrity failure, an unknown schematic or version, or ten failures |
| `config_unrecorded` | `alert.config_unrecorded` | A config was delivered and could not be recorded, even after retries; the runbook depends on the machine's state, see [Install Talos](first-install.md#when-something-goes-wrong) |
| `git_store_moved_aside` | `cluster.store_moved_aside` | The git object store was corrupt and was moved to `git.corrupt-<UTC time>`; at most once an hour. A pair whose tree is no longer upstream cannot render until a new one is approved |
| `cmdline_too_long` | `alert.cmdline_too_long` | The install kernel command line would exceed 2046 bytes, so the machine holds; once per machine per hour |

## Logs

The `/config` request carries its ticket in the `t=` query parameter, which the request log replaces with `[redacted]` together with the rest of the query string. A reverse proxy in front of `proxy` mode logs its own copy: configure it to redact query strings (see [TLS](tls.md#proxy-mode)). Errors from resolving, loading and rendering a definition are logged by stage, file and rule only; the text that could quote a secret is stored sealed and printed only by `cluster list --errors`.

[← Back to README](../README.md)
