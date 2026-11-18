# Observability

This page is the reference for what the admin listener reports: its routes, the `/health` items and the metrics.

The admin listener is on `HEPHAESTUS_ADMIN_ADDRESS` (default `127.0.0.1:9090`) and serves only `GET /healthz`, `GET /readyz`, `GET /health` and `GET /metrics`. `/healthz` fails only once the service has stopped; `/readyz` is 200 only when the service is ready and every critical check passes (add `?verbose` to list every check); `/health` is a health+json document that returns 503 when a critical check fails.

Other checks: `tls:certificate` and `tls:expiry` are described in [TLS](tls.md), and `/health` warns on `disk:data_dir` when the data directory's disk has less than 10% free.

**`/health` items** (`GET /health` on the admin listener):

| Item | Meaning |
|---|---|
| `store:readable` | Critical. Passes when `index.json` decodes and the blob directory lists; a failure takes `/readyz` to 503 |
| `store:bytes` | Informational. The total size of the blob files, as an observed value in bytes |

**Metrics** (`GET /metrics` on the admin listener):

| Metric | Meaning |
|---|---|
| `hephaestus_utility_fetch_bytes_total` | Bytes downloaded from upstreams |
| `hephaestus_utility_fetch_failures_total{class}` | Failed fetches, by class `integrity` or `other` |
| `hephaestus_utility_boots_total{entry}` | Utility boot scripts served |

For the fetch log lines, see [Utility content](utility-content.md#fetch-log-lines).

[← Back to README](../README.md)
