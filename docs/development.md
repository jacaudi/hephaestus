# Development

This page is the task guide for building, testing and checking hephaestus from source: the prerequisites and the `task` commands.

## Prerequisites

- Go 1.26+
- [Task](https://taskfile.dev)
- Docker, through colima, for the Postgres tests and the iPXE builds
- hadolint

With colima, set `DOCKER_HOST=unix:///Users/<you>/.colima/default/docker.sock`. The Dockerfile builds linux/amd64 and linux/arm64 by cross-compiling on the build platform; the final image is distroless and runs as `65532:65532`.

## Tasks

```sh
task build         # build bin/hephaestus
task ci            # fmt and tidy checks, the machinery and no-os/exec checks, lint, vuln, render parity,
                   # hadolint, go test -race, the 1Password SDK smoke test, cross-compile,
                   # Postgres integration tests, coverage gate (needs DOCKER_HOST)
task render:parity # golden parity of the render against `topf render` at the pinned commit (needs DOCKER_HOST)
task ipxe:build    # rebuild the embedded iPXE binaries (needs DOCKER_HOST)
task image:build   # build hephaestus:dev for linux/arm64; PLATFORM=linux/amd64 overrides (needs DOCKER_HOST)
task e2e           # boot QEMU VMs through hephaestus in a Linux Docker daemon, utility boots and a Talos install (M3 phase untimed so far; the M1 and M2 phases take about 5 to 6 minutes with the layer cache warm), not part of ci (colima needs DOCKER_HOST; see test/e2e/README.md)
task catalog:pin   # re-pin the utility catalog; see the catalog page
```

`task --list` shows every task. Four checks in `task ci` guard the vendored Topf render path: `go:machinery` keeps `github.com/siderolabs/talos/pkg/machinery` at v1.14.4 and refuses the `talos` module, `go:noexec` fails if the vendored code imports `os/exec`, `render:parity` runs `topf render`, built from a git checkout of the pinned commit, against hephaestus on the same fixtures and requires byte-identical output, and `go:sdk-smoke` builds the real 1Password client without cgo and requires a dummy token to fail with an error, not a panic. The vendored code is described in `internal/third_party/topf/VENDORED.md`. The catalog task is described in [Utility catalog](utility-catalog.md). The end-to-end harness is described in [its own README](../test/e2e/README.md). For real-hardware checks that `task e2e` cannot cover, see the [M1](lab/m1-bring-up.md) and [M2](lab/m2-utility-boots.md) lab bring-ups.

[← Back to README](../README.md)
