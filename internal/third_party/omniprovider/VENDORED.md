# Vendored: Omni bare-metal infrastructure provider (proxyDHCP and TFTP)

| | |
|---|---|
| Source | https://github.com/siderolabs/omni-infra-provider-bare-metal |
| Tag | `v0.15.0` |
| Commit | `13d64d3ec8e1a4f90f50af160380b086030f2343` |
| Licence | MPL-2.0, `LICENSE` in this directory (the provider's `LICENSE`, verbatim) |

## Files

| Here | Upstream path |
|---|---|
| `dhcp/dhcp.go` | `pkg/dhcp/dhcp.go` |
| `dhcp/proxy.go` | `pkg/dhcp/proxy.go` |
| `dhcp/export_test.go` | `pkg/dhcp/export_test.go` |
| `dhcp/proxy_test.go` | `pkg/dhcp/proxy_test.go` |
| `tftp/tftp_server.go` | `pkg/tftp/tftp_server.go` |

Each file keeps its MPL-2.0 header; the line directly under it is a `hephaestus:` provenance comment naming the source and that file's changes.

Not copied: `pkg/dhcp/boot_contract_test.go`. It imports provider-internal packages (`internal/provider/ipxe`, `pkg/ipxe`). `dhcp/contract_test.go` (original to this repository, not vendored) replaces it and checks the advertised boot files against `internal/boot/ipxe.Files()`.

## Changes from upstream

1. `go.uber.org/zap` replaced by `log/slog` in `proxy.go` and `tftp_server.go`: `*zap.Logger` is `*slog.Logger`, `zap.String`, `zap.Int` and `zap.Int64` are the `slog` equivalents, `zap.Error(err)` is `slog.Any("error", err)`. Each call keeps its upstream level and message.
2. `proxy.go`: the `github.com/siderolabs/gen/xslices` `Map` call in `validateDHCP` replaced by a plain loop building the same `[]string`; the `siderolabs/gen` dependency is dropped.
3. `proxy_test.go`: the import of the provider's own `github.com/siderolabs/omni-infra-provider-bare-metal/pkg/dhcp` rewritten to `go.jacaudi.dev/hephaestus/internal/third_party/omniprovider/dhcp`.
4. `dhcp.go`, `export_test.go`: unchanged apart from the provenance line.

Tests keep `testify`. Upstream log levels are kept: this directory is exempt from the "no Info or above in `internal/**`" rule.

## Linting

`golangci-lint` skips `internal/third_party/` (`linters.exclusions.paths` and `formatters.exclusions.paths` in `.golangci.yml`).

## Notes

The TFTP server serves the byte slices it is given and never modifies them. `ipxe.Files()` returns slices that alias the embedded binaries, so that read-only behaviour is required.
