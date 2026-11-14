<!-- Draft for an issue on the owner's fork jacaudi/topf (D20). Not posted: gh token invalid on 2026-11-13. -->
# Expose per-node config rendering as a public package

## Problem

The fleet service (the successor to booty) has to render Talos machine configs
byte-for-byte as `topf render` does. It imports a Topf repo (`topf.yaml` plus
patch directories) and serves each approved node its config at
`/config?u=${uuid}`. The service stores a hash of the last approved config, so
any difference from Topf's output counts as drift.

Nothing that renders a node's config can be imported at the moment:

- `generateNodeConfig` is in `internal/topf/nodes.go:136` (at `70cdbc8`, the commit the `v0.6.2` git tag points to).
- `pkg/config` is public, but `PatchContext.Load` needs `*decryption.Cache` from
  `internal/`. With a nil cache, loading any patch that is not `.tpl` panics. A probe
  reproduced this (local research notes, talos-booter `docs/research/booty-slim/sources/11-topf.md` §7).
- The version fallback chain (running → `topf.yaml talosVersion` → bundled
  `version.Tag`) is in `internal/topf/node.go:47-49`.

## Proposed change

Add a public package, for example `pkg/render`, that exposes:

1. `Render(ctx, cfg *config.TopfConfig, node config.Node, bundle *secrets.Bundle, opts ...Option) ([]byte, error)`,
   with the same output as `topf render` for that node.
2. A decryption hook as an interface, so the caller supplies the decryption (SOPS, vals,
   or none) and `pkg/config` stops depending on `internal/`.
3. Explicit contract selection (`WithContract(version)`), so the caller pins the
   version contract and nothing is inferred from a running node.

`topf render` and `topf apply` keep their behaviour by calling the new package.

## Acceptance

- `topf render` output is unchanged (a golden test over `example/`).
- An external module can render a node without importing `internal/`.
- No new behaviour for existing CLI users.

## Notes

- Interim: the service vendor-copies the render path at a pinned commit and runs a CI
  golden test against `topf render`. This fork carries the change until it is ready to offer upstream.
- Pin by commit. `proxy.golang.org` resolves `v0.6.2` to `7aeead7`, but the git tag now
  points to `70cdbc8`.
