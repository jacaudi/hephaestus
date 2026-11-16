# hephaestus — iPXE TLS session-ticket fix and the `task e2e` harness

**Spec:** docs/designs/2026-11-13-hephaestus-core-design.md (§7 criterion 6, §11 lab tests 1 and 7)

**Goal:** Make HTTPS boot work with iPXE `9261ebe`, then replace the `task e2e` stub with a repeatable QEMU harness that boots amd64 UEFI, amd64 BIOS and arm64 UEFI through hephaestus in colima's Linux VM, in file mode and proxy mode.

**Evidence:** the feasibility spike, `~/.cache/hephaestus-e2e-spike/SPIKE.md` (2026-11-16), is the source for every command, package and quirk below.
- Go's TLS 1.3 server sends NewSessionTicket before it reads the client Finished.
- iPXE `net/tls.c` at 9261ebe hashes every handshake message until the handshake completes, so the ticket lands in its transcript. The result is `tls: invalid client finished hash`, then `invalid PSK binder` on each retry.
- `SessionTicketsDisabled: true` fixes it. The A/B test passed on all three firmwares and through Caddy with its tickets off.

**Sizing:** small change against an approved design (§7.6 already specifies the harness). No new design gate. Each task gets a builder and an Opus critic.

**Run contract:** merge locally into main after `task ci` and `task e2e` pass; remove the worktree afterwards. Commits are signed (the M1 unsigned directive has ended); a signing failure pauses the run. No remote, no push. Nothing is filed upstream with iPXE without the owner's explicit permission. Builders run on Sonnet, critics on Opus. Two fix rounds per task.

## Global Constraints
- Prefix commands with `rtk proxy`; Docker needs `DOCKER_HOST=unix://$HOME/.colima/default/docker.sock`. Colima shares only `$HOME`.
- Nothing is installed on the macOS host. Every tool runs in a container except `task`, `docker`, `git` and the Go toolchain.
- Pin every container image by digest and every downloaded package by sha256.
- TDD for Go behaviour. Shell harness steps are verified by running `task e2e`.
- Commit trailer: `Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>`. Stage explicit paths only.

### Task 1: Disable TLS session tickets on the boot HTTPS listener
Kind: rewrite

**Files:** Modify `cmd/hephaestus/serve.go`, `cmd/hephaestus/serve_test.go`, `README.md`, `docs/designs/2026-11-13-hephaestus-core-design.md`, `docs/research/booty-slim/README.md`.

- `serve.go`: set `SessionTicketsDisabled: true` on the boot HTTPS `tls.Config`, with a comment that names the iPXE 9261ebe transcript behaviour. The comment states the behaviour, not a bug ID.
- Test `TestRunServe_NoSessionTickets`:
  - file mode;
  - two TLS 1.3 connections to the HTTPS listener using a client `tls.Config` with `ClientSessionCache: tls.NewLRUClientSessionCache(4)`;
  - each connection completes a `GET /boot?<ServesAndDrains query>` and reads the body;
  - assert the second connection's `ConnectionState().DidResume == false`;
  - goleak.
  RED first: without the server change, the second connection resumes.
- README proxy mode: the proxy must also disable TLS session tickets toward iPXE. Add `"session_tickets": {"disabled": true}` to the Caddyfile/JSON example, in the Caddy form that actually works (the spike used `apps.tls.session_tickets.disabled`). Add one line saying why.
- Design:
  - §5.10 (TLS / iPXE): session tickets are off, with the reason.
  - §11 lab test 1: record the 2026-11-16 finding and that QEMU firmware covers the handshake now. Physical NIC firmware and the ISRG chain with egress blocked remain lab work.
  - The proxy-mode bullet in §6: tickets must be off at the proxy.
- Research record: append `D26 — boot TLS 1.3 runs with session tickets disabled: iPXE 9261ebe hashes a pre-Finished NewSessionTicket into its transcript (found by the e2e spike, 2026-11-16)`.

Run: `rtk proxy go test ./cmd/hephaestus/ -run TestRunServe_NoSessionTickets -race -count=1`. Expected: FAIL before the change, PASS after.
Run: `DOCKER_HOST=… rtk proxy task ci`. Expected: exit 0.
Commit: `fix: disable TLS session tickets for iPXE on the boot HTTPS listener`.

### Task 2: `task e2e`, the QEMU boot harness
Kind: rewrite

**Files:**
- Create: `test/e2e/run.sh` (POSIX sh driver), `test/e2e/gen-certs.sh`, `test/e2e/dnsmasq.conf`, `test/e2e/caddy.json`, `test/e2e/qemu/Dockerfile`, `test/e2e/qemu/run-vm.sh`, `test/e2e/README.md`.
- Modify: `taskfile.yml` (the `e2e` task), `.gitignore`, `.dockerignore`, `docs/designs/2026-11-13-hephaestus-core-design.md`.

Pinned decisions:
- **Topology** (the spike's, §3–§6 of SPIKE.md):
  - docker network `heph-e2e`, subnet `10.99.0.0/24`;
  - dnsmasq at `10.99.0.2`, giving DHCP and DNS (`hephaestus.e2e.test`) with no PXE options;
  - hephaestus at `10.99.0.10`, file mode, provisioning interface `eth0`;
  - QEMU container with a tap bridged to its `eth0`;
  - for proxy mode, Caddy at `10.99.0.11`, with hephaestus in proxy mode behind it and `HEPHAESTUS_PROXY_TRUSTED=10.99.0.11/32`.
  The plain `docker run` driver is used; compose adds nothing.
- **Images:** dnsmasq, Caddy and the QEMU base image are pinned by digest. The QEMU image installs the spike's Debian packages plus Ubuntu's `ovmf_2024.02-2ubuntu0.10_all.deb`, verified by sha256 (Debian trixie's OVMF has no PXE stack). Resolve each digest and checksum once with `docker buildx imagetools inspect` / `sha256sum` and record it in the file.
- **Test CA:** generated fresh each run, inside a container, by `gen-certs.sh`, into `build/e2e/run/certs/` (git-ignored). Copy the CA PEM to `build/ipxe/out/e2e-ca.pem` for `task ipxe:build:testca TEST_CA=build/ipxe/out/e2e-ca.pem`. If build.sh requires a different relative form, use it and say so.
- **e2e image** `hephaestus:e2e`, built from a temporary copy of the tracked working tree (`git ls-files -z | tar --null -T - -cf - | tar -xf - -C <tmp>`), with `internal/boot/ipxe/bin/` overlaid by `build/ipxe/out/testca/*`, using the unmodified production Dockerfile. The production Dockerfile and the repo tree never hold test-CA binaries. Delete the temp copy at exit.
- **Seed:** before booting, set `unknown_action` to `hold` through a one-off sqlite container on the data volume, after hephaestus has migrated the schema. Each VM gets its own `-uuid` and MAC.
- **Boots**, each with a 120 s timeout, in this order: `ovmf`, `seabios`, `aarch64` (file mode), then `ovmf` (proxy mode). Stop each boot at its first hold.
- **Assertions per boot** (SPIKE.md "Pass/fail assertions" 1–5): proxyDHCP offers the expected boot file and TFTP sends it; `/start` 200 with `tls:false`; `/boot` 200 with `tls:true`, `decision_row:7`, `action:"hold"`; `POST /discover` 200 `stored`; zero `TLS handshake error` lines; serial-console holds on UEFI; one `discovered` sqlite row per MAC with `junk=0` and the right arch; the `/metrics` hold counter ≥ 1.
- **Run-level negative checks:**
  - `openssl s_client -tls1_2` gets a handshake alert;
  - `--cap-drop ALL` without the cap-add fails to exec;
  - with `--cap-drop ALL --cap-add NET_BIND_SERVICE --sysctl net.ipv4.ip_unprivileged_port_start=1024`, all four ports bind as 65532;
  - proxy mode: a direct request from a non-proxy peer gets 403.
  `no-new-privileges` is not asserted: Docker grants CapEff directly, so it does not isolate the file capability (SPIKE.md §5). The README says so.
- **Output:** every container log, serial log and the sqlite dump goes to `build/e2e/<UTC timestamp>/` (git-ignored, also in `.dockerignore`). It prints a PASS/FAIL line per boot and per check, and exits non-zero on any failure. A `trap` removes every `heph-e2e-*` container, the network and the volume on any exit.
- **taskfile:** `e2e` replaces the stub, keeps `desc` accurate (colima/Linux Docker, about 3 min cold), stays out of `ci`, and `requires` `DOCKER_HOST`.
- **Design §7 criterion 6:** `task e2e` exists and runs in colima's Linux VM. It covers iPXE → `/start` → `/boot` → `/discover` and the hold for M1. Approval, install script and kernel fetch arrive with M2/M3. Lab test 7: the capability part is covered by e2e; TrueNAS defaults remain lab work.

Run: `DOCKER_HOST=… rtk proxy task e2e`. Expected: every boot and check PASS; exit 0; about 3 min cold, about 1.5 min warm; afterwards `docker ps -a --filter name=heph-e2e` and `docker network ls --filter name=heph-e2e` are empty.
Run it a second time. Expected: PASS again (warm).
Run: `DOCKER_HOST=… rtk proxy task ci`. Expected: exit 0; hadolint covers `test/e2e/qemu/Dockerfile`.
Commit: `test: task e2e boots amd64 UEFI, BIOS and arm64 UEFI through hephaestus in QEMU`.

## Review Focus
- Task 1: the test proves that no session ticket is issued, not merely that a handshake succeeds.
- Task 2: no assertion can pass vacuously (an empty log must FAIL). Cleanup runs on failure and on Ctrl-C. No test-CA binary reaches the repo tree or the production image. Every pin is real.
