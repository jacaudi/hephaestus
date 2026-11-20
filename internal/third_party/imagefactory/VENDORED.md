# Vendored: image-factory schematic model

| | |
|---|---|
| Source | https://github.com/siderolabs/image-factory |
| Module version | `v1.7.1` (git `77ecb9ec4da817df82cf0eeea54069ecc6acf6a1`) |
| Licence | MPL-2.0, `LICENSE` in this directory (image-factory's `LICENSE`, verbatim) |

The module is not a dependency of this repository (it is not in `go.mod`); the file was copied from the module cache directory printed by `go mod download -json github.com/siderolabs/image-factory@v1.7.1`.

## Files

| Here | Upstream path | Status |
|---|---|---|
| `schematic/schematic.go` | `pkg/schematic/schematic.go` | unchanged, byte for byte, MPL-2.0 header kept |

Written for hephaestus (header `// hephaestus: new`), not image-factory's: `schematic/canonical.go` and `schematic/canonical_test.go`.

Not copied: `pkg/schematic/schematic_test.go`, and everything else of image-factory.

## Changes from upstream

1. `schematic.go`: none. It imports `github.com/siderolabs/gen/xerrors` (required at the version the machinery v1.14.4 module graph selects, `v0.8.8`), `github.com/siderolabs/talos/pkg/machinery/imager/imageropts` and `go.yaml.in/yaml/v4`.
2. New `canonical.go`: `Canonical(body)` reads a schematic with `Unmarshal` (unknown fields are an error) and writes it back with `(*Schematic).Marshal`, the bytes image-factory hashes; `ID(body)` is the hex SHA-256 of those bytes, equal to `(*Schematic).ID()`. The id of the empty schematic `customization: {}` is `376567988ad370138ad8b2698212367b8edcb69b5fd68c80be1f2ec7d603b4ba`.

## Linting

`golangci-lint` skips `internal/third_party/` (`linters.exclusions.paths` and `formatters.exclusions.paths` in `.golangci.yml`).
