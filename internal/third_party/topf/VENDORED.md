# Vendored: Topf cluster config loader (pkg/config)

| | |
|---|---|
| Source | https://github.com/postfinance/topf |
| Commit | `70cdbc83cfbadc023ce4c7c0e3180eedfa577f16` (2026-10-23) |
| Licence | MIT, `LICENSE` in this directory (Topf's `LICENSE`, verbatim) |

## Pinning

Topf is taken from a git checkout of the full SHA above, never through the Go module proxy, never by tag. The git tag `v0.6.2` points at `70cdbc83…` (machinery v1.14.4), but the proxy serves `v0.6.2` as `7aeead75…` (the commit now tagged `v0.6.2-rc.0`, machinery v1.14.1), because the tag moved after the proxy cached it. `go install …@v0.6.2` or `@70cdbc8` through the proxy builds the wrong tree. The checkout's `go.mod` names `github.com/siderolabs/talos/pkg/machinery v1.14.4`, `github.com/Masterminds/sprig/v3 v3.3.0`, `go.yaml.in/yaml/v4 v4.0.0-rc.6.0.20260809190231-643e93b9c9be` and `gopkg.in/yaml.v3 v3.0.1`; this repository's `go.mod` requires the same versions. The checkout is kept outside the repository, under `$HOME/.cache/hephaestus/topf-70cdbc83`.

## Files

| Here | Upstream path | Status |
|---|---|---|
| `config/topf_config.go` | `pkg/config/topf_config.go` | changed |
| `config/patches.go` | `pkg/config/patches.go` | changed |
| `config/node.go` | `pkg/config/node.go` | unchanged |
| `config/node_role.go` | `pkg/config/node_role.go` | unchanged |
| `config/endpoint.go` | `pkg/config/endpoint.go` | unchanged |

The changed files keep Topf's MIT header; the line directly under it is a `hephaestus:` provenance comment, and each change is marked `// hephaestus: changed — <what>` at the code. The unchanged files are byte-for-byte Topf's.

Written for hephaestus (header `// hephaestus: new`), not Topf's: `config/doc.go`, `config/fsys.go`, `config/refs.go`, `config/template.go`, and the tests `config/config_test.go`, `config/refs_test.go`, `config/fsys_test.go`.

Not copied: `pkg/config/*_test.go` (they use host paths), and everything else of Topf.

## Changes from upstream

1. `topf_config.go`: `LoadFromFile(path string) (*TopfConfig, []string, error)` becomes `Load(fsys fs.FS, values map[string]string) (*TopfConfig, error)`. It reads `topf.yaml` at the root of `fsys` through `ReadResolved` and decodes it with `go.yaml.in/yaml/v4` exactly as Topf does. The `configDir` refusal and the node sort (control plane first, then host name) are kept.
2. `topf_config.go`: a set `nodesProvider` is refused with `topf: nodesProvider is not supported`. `SecretsProvider` and `NodesProvider` stay as fields so the renderer can refuse them.
3. `topf_config.go`: `PatchesDir` defaults to `.` and `SecretsPath` to `secrets.yaml`, relative to the tree root (Topf resolves them against the directory of the config file); a set value goes through `CleanRel` and must stay inside the tree.
4. `topf_config.go`: the SOPS mask list (`[]string`) is not returned; `GetSecretsProvider` and the `decryption` and `providers` imports are removed; the package comment moves to `doc.go`.
5. `patches.go`: `PatchContext` loses `DecryptCache` and gains `FS fs.FS`, `Values map[string]string` and `Funcs template.FuncMap`. `Load()` returns no mask list and reads the folders `<PatchesDir>/all`, `<PatchesDir>/<role>` and `<PatchesDir>/node/<host>` from `FS`.
6. `patches.go`: the folder walk is `collectPatchFiles(fsys, folder)`, over `fs.WalkDir`. The file pattern `.*\.ya?ml(\.tpl)?` is kept, unanchored. A symlink is resolved as Topf does, through `EvalSymlinks` and `fs.Stat`: a linked file is collected under its link name when the pattern matches its target's name, a linked directory is walked under its link name, and a link whose target leaves the tree is `ErrEscapes`.
7. `patches.go`: a linked directory already on the walk chain is `ErrSymlinkLoop`. Topf recurses until the path is too long.
8. `patches.go`: only a missing folder yields no files. Topf's `os.IsNotExist` on the whole walk also emptied a folder that held a dangling symlink, silently dropping every patch in it. Here only a folder missing as a plain path element has no files; a symlink on the way (the patches directory, `node`, a folder, or any link inside) whose target does not exist is an error matching `fs.ErrNotExist`, and one that leaves the tree is `ErrEscapes`.
9. `patches.go`: `RenderTemplate(fsys fs.FS, name string, funcs template.FuncMap, data any)` reads through `fs.ReadFile` and takes the function map as a parameter, where Topf fixed it to sprig's `TxtFuncMap()`. `missingkey=error` is kept.
10. `patches.go`: `loadFile` renders a `.tpl` file with `TemplateData()` (no reference substitution, as Topf) and reads every other file with `ReadResolved`. `parsePatches` is unchanged.
11. New `fsys.go`: `ErrEscapes`, `ErrSymlinkLoop`, `CleanRel` and `EvalSymlinks` (resolves symlinks of an `fs.FS` that implements `fs.ReadLinkFS`, at most 40 per path).
12. New `refs.go`: `SubstituteReferences` replaces Topf's `vals eval` pipe (`internal/decryption` and `internal/vals`). It decides whether a file holds a reference as Topf's `hasValsRefs` does (`gopkg.in/yaml.v3` decode into `any`, stop at the first decode error); a file holding none is returned byte for byte. Otherwise each reference scalar (never a mapping key) is replaced by its value from the map as a string, and the documents are encoded again with indent 2. A reference with no value is a `*MissingReferenceError`. `HasReferencePrefix` is Topf's pattern `^ref\+[a-zA-Z0-9]+://` (`internal/vals/vals.go`), written without a package-level regexp.
13. New `template.go`: `TemplateData` and `TemplateNode`, what a template executes against. `TemplateData` has no methods and `TemplateNode` has `Endpoint` only (a template also reaches the methods of the `netip.Addr` behind `Node.IP`); field names equal to `PatchContext`'s and `Node`'s so a Topf template renders the same text; `PatchesDir`, `DecryptCache`, `FS`, `Values`, `Funcs` and `Load` are not reachable from a template. `Node.IP` is a fresh copy per render, and `RuntimeData` is always zero.

## Linting

`golangci-lint` skips `internal/third_party/` (`linters.exclusions.paths` and `formatters.exclusions.paths` in `.golangci.yml`). `task go:noexec` fails if any package here imports `os/exec`, and `task go:machinery` asserts `pkg/machinery` stays at v1.14.4 with no `github.com/siderolabs/talos` main-module dependency.
