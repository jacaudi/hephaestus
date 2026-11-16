# End-to-end boot test

`task e2e` boots real firmware in QEMU through hephaestus, over a bridged L2 network in colima's Linux VM, and checks each leg:
firmware PXE, proxyDHCP, TFTP, the embedded iPXE script, `/start`, HTTPS `/boot` (TLS 1.3), `/discover`, and the M1 hold.

```sh
DOCKER_HOST=unix://$HOME/.colima/default/docker.sock task e2e
```

`DOCKER_HOST` is only needed when your Docker daemon is not the default one (colima's socket is not); if no daemon is reachable
the script says so and names colima's socket. The harness has been run on colima on arm64 only; it picks `linux/arm64` or
`linux/amd64` from the daemon's architecture, but amd64 hosts are untested.

It needs `task`, `docker` and `git` on the host and nothing else: openssl, sqlite, dnsmasq, QEMU, curl and jq run in containers.
It takes about 2.5 minutes with the BuildKit layer cache warm and the images removed (measured: 154 s and 150 s); the test CA is new
every run, so iPXE and the `hephaestus:e2e` image are rebuilt every run, and a warm run is no faster. Here "cold" means the images were
removed and the layers kept; a first-ever run on a fresh daemon downloads more and is untimed. It is not part of `task ci`. VMs run
under TCG, with no KVM.

The container names, the `heph-e2e` network, its `10.99.0.0/24` subnet and the `heph-e2e-data` volume are fixed, so two runs at
the same time on one daemon conflict (the second removes the first's containers). Run one at a time.

## What it does

1. Builds the toolbox image (`qemu/Dockerfile`), generates a test CA and a leaf for `hephaestus.e2e.test` (`gen-certs.sh`),
   and builds iPXE trusting only that CA (`task ipxe:build:testca`).
2. Builds `hephaestus:e2e` with the unmodified production `Dockerfile` from a temporary copy of the tracked tree, with
   `internal/boot/ipxe/bin/` overlaid by the test-CA binaries and `roots.pem`. The temporary copy is deleted straight after
   the build, so no test-CA binary reaches the repository tree or the production image.
3. Creates network `heph-e2e` (`10.99.0.0/24`): dnsmasq at `.2` (DHCP and DNS, no PXE options), hephaestus at `.10`
   (file mode), the QEMU toolbox at `.20`, and in proxy mode Caddy at `.11` (`caddy.json`). The VM's tap is bridged to the
   toolbox's `eth0`, which puts it on the same broadcast domain as dnsmasq and hephaestus.
4. Sets `unknown_action` to `hold` in the SQLite database (M1 has no UI), then boots, one at a time, each with a 120 s
   timeout, stopping at the first hold: `ovmf` (amd64 UEFI), `seabios` (amd64 BIOS), `aarch64` (arm64 UEFI), all in file
   mode, then `ovmf` again in proxy mode. Each VM gets its own MAC and `-uuid`.

## Checks

Per boot: the VM reached its hold; dnsmasq leased it an address; proxyDHCP offered the expected boot file and TFTP sent it;
`/start` was 200 over plain HTTP; `/boot` was 200 with decision row 7 and action `hold`, over TLS; `POST /discover` was 200
`stored`; no `TLS handshake error` line; on UEFI, the serial console shows the discovery report completing and
`hephaestus: holding: awaiting approval`; SQLite has exactly one `discovered` row for the MAC with `junk=0`, the right
architecture and the VM's UUID; `hephaestus_boot_decisions_total{action="hold",decision_row="7"}` rose. SeaBIOS and its iPXE ROM
write to VGA only, so the BIOS boot is judged from the server log. In proxy mode Caddy's access log must also show the `/boot`
request as TLS 1.3, and hephaestus sees the client in `client_ip`.

Run-level: `openssl s_client -tls1_2` gets a protocol-version alert; a TLS 1.3 handshake verifies against the test CA and no
session ticket arrives (iPXE `9261ebe` fails the handshake on a ticket sent early); `--cap-drop ALL` without the cap-add fails to
exec; with `--cap-drop ALL --cap-add NET_BIND_SERVICE --sysctl net.ipv4.ip_unprivileged_port_start=1024` hephaestus binds UDP 67,
69, 4011 and TCP 80, 443 as UID 65532; in proxy mode a direct request to `:8443` from a peer that is not the proxy gets 403.
The sysctl matters because Docker sets `net.ipv4.ip_unprivileged_port_start` to 0 in container network namespaces, which lets any
user bind low ports and would hide whether the capability is what binds them; 1024 restores the host default.

The "zero TLS handshake error lines" check only means something if the TLS terminator can print that line, so each mode has a
positive control first: the refused TLS 1.2 probe must leave exactly one `TLS handshake error` line in the container's full log
(Caddy needs `logging` at DEBUG for its `http.stdlib` logger to emit it; `caddy.json` sets that). The boot checks then read only the
log written after the probe.

No check passes on an empty log: each one reads a captured file and fails if the file is empty or the line it looks for is missing.
`no-new-privileges` is deliberately not asserted: Docker grants the added capability to the non-root process directly, so the flag
does not isolate the file capability. TrueNAS defaults stay lab work (design §11, lab test 7).

## Output and cleanup

Everything lands in `build/e2e/<UTC timestamp>/` (git-ignored): every container log, every serial log, the TLS probes, the SQLite
dump. It prints a `PASS` or `FAIL` line per boot and per check and exits non-zero on any failure. A trap on exit, SIGINT and
SIGTERM removes every `heph-e2e-*` container, the network, the volume, the temporary source copy and the certificates, and the
test-CA build output under `build/ipxe/out` only if this run created it (it never deletes files that were already there). If a run
was killed with SIGKILL, the next run removes the containers, network and volume it left; the git-ignored files it left under
`build/ipxe/out` are overwritten by the next run.

## Pins

The toolbox and the iPXE builder start from `debian:trixie-slim` by digest and install packages from the dated Debian snapshot
`20261104T212339Z`. Debian trixie's OVMF has no network boot stack, so the amd64 UEFI firmware is Ubuntu noble's
`ovmf_2024.02-2ubuntu0.10_all.deb`, checked by sha256 (`qemu/Dockerfile`). arm64 uses Debian's AAVMF, which does PXE. Caddy is
pinned by digest in `run.sh`.
