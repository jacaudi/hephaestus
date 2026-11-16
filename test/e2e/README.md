# End-to-end boot test

`task e2e` boots real firmware in QEMU through hephaestus, over a bridged L2 network in colima's Linux VM, and checks each leg:
firmware PXE, proxyDHCP, TFTP, the embedded iPXE script, `/start`, HTTPS `/boot` (TLS 1.3), `/discover`, and the M1 hold.

```sh
DOCKER_HOST=unix://$HOME/.colima/default/docker.sock task e2e
```

It needs `task`, `docker`, `git` and Go on the host and nothing else: openssl, sqlite, dnsmasq, QEMU, curl and jq run in containers.
It takes about 3 minutes from a cold Docker cache and a little longer or shorter warm: the test CA is new every run, so iPXE
and the `hephaestus:e2e` image are rebuilt every run. It is not part of `task ci`. VMs run under TCG, with no KVM.

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
exec; with `--cap-drop ALL --cap-add NET_BIND_SERVICE` hephaestus binds UDP 67, 69, 4011 and TCP 80, 443 as UID 65532; in proxy
mode a direct request to `:8443` from a peer that is not the proxy gets 403.

No check passes on an empty log: each one reads a captured file and fails if the file is empty or the line it looks for is missing.
`no-new-privileges` is deliberately not asserted: Docker grants the added capability to the non-root process directly, so the flag
does not isolate the file capability. TrueNAS defaults stay lab work (design §11, lab test 7).

## Output and cleanup

Everything lands in `build/e2e/<UTC timestamp>/` (git-ignored): every container log, every serial log, the TLS probes, the SQLite
dump. It prints a `PASS` or `FAIL` line per boot and per check and exits non-zero on any failure. A trap on exit, SIGINT and
SIGTERM removes every `heph-e2e-*` container, the network, the volume, the temporary source copy, the certificates and the test-CA
build output. If a run was killed with SIGKILL, the next run removes what it left.

## Pins

The toolbox and the iPXE builder start from `debian:trixie-slim` by digest and install packages from the dated Debian snapshot
`20261104T212339Z`. Debian trixie's OVMF has no network boot stack, so the amd64 UEFI firmware is Ubuntu noble's
`ovmf_2024.02-2ubuntu0.10_all.deb`, checked by sha256 (`qemu/Dockerfile`). arm64 uses Debian's AAVMF, which does PXE. Caddy is
pinned by digest in `run.sh`.
