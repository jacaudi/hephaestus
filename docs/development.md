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
task ci            # fmt and tidy checks, lint, vuln, hadolint, go test -race, cross-compile,
                   # Postgres integration tests, coverage gate (needs DOCKER_HOST)
task ipxe:build    # rebuild the embedded iPXE binaries (needs DOCKER_HOST)
task image:build   # build hephaestus:dev for linux/arm64; PLATFORM=linux/amd64 overrides (needs DOCKER_HOST)
task e2e           # boot QEMU VMs through hephaestus in a Linux Docker daemon; about 5 to 6 minutes with the layer cache warm, not part of ci (colima needs DOCKER_HOST; see test/e2e/README.md)
task catalog:pin   # re-pin the utility catalog; see the catalog page
```

`task --list` shows every task. The catalog task is described in [Utility catalog](utility-catalog.md). The end-to-end harness is described in [its own README](../test/e2e/README.md). For real-hardware checks that `task e2e` cannot cover, see the [M1](lab/m1-bring-up.md) and [M2](lab/m2-utility-boots.md) lab bring-ups.

[← Back to README](../README.md)
