# End-to-end boot test

`task e2e` boots real firmware in QEMU through hephaestus, over a bridged L2 network in colima's Linux VM, and checks each leg:
firmware PXE, proxyDHCP, TFTP, the embedded iPXE script, `/start`, HTTPS `/boot` (TLS 1.3), `/discover`, and the M1 hold. Its
M2 utility phase then fetches test catalog entries from a local TLS 1.3 fake upstream into hephaestus's content store and boots
them: an EFI chain, a kernel with an initrd and a squashfs, and an entry the firmware cannot boot. Its M3 install phase imports a
fixture cluster from a local git host, approves its first pair and one machine into it, and boots that machine to its install
script: hephaestus fetches the Talos kernel, initramfs and command line from a local fake image factory, following its
redirects to a second host name, and serves them to the VM; the harness then fetches `/config` with the machine's ticket.

```sh
DOCKER_HOST=unix://$HOME/.colima/default/docker.sock task e2e
```

`DOCKER_HOST` is only needed when your Docker daemon is not the default one (colima's socket is not); if no daemon is reachable
the script says so and names colima's socket. The harness has been run on colima on arm64 only; it picks `linux/arm64` or
`linux/amd64` from the daemon's architecture, but amd64 hosts are untested. The M3 install phase has not been timed yet: it adds
the upstreams program's `go run` (in a fresh container, so it compiles every run) and one install boot of up to 300 s.

It needs `task`, `docker` and `git` on the host and nothing else: openssl, sqlite, dnsmasq, QEMU, curl and jq run in containers.
It takes about 5 to 6 minutes with the BuildKit layer cache warm (measured with the M2 utility phase: 290 to 369 s; the M1 phase alone took
150 to 154 s). The test CA is new every run, so iPXE and the final `hephaestus:e2e` image build are rerun every run, and a warm run is
no faster. The pin tool's image and `go mod download` come from the cache: the test-CA binaries enter the tree only after the pin image
is built, and the image's build date is the HEAD commit's, not the run's. A first-ever run on a fresh daemon downloads more (the Debian
snapshot, the fixture's kernel packages and the Go toolchain image) and is untimed. It is not part of `task ci`. VMs run under TCG, with
no KVM. The Docker daemon needs room for the images and build cache (see `docker buildx du`).

The container names, the `heph-e2e` network, its `10.99.0.0/24` subnet and the `heph-e2e-data` volume are fixed, so two runs at
the same time on one daemon conflict (the second removes the first's containers). Run one at a time.

## What it does

1. Builds the toolbox image (`qemu/Dockerfile`), generates a test CA and leaves for `hephaestus.e2e.test`, `upstream.e2e.test`,
   `factory.e2e.test`, `assets.factory.e2e.test` and `git.e2e.test` (`gen-certs.sh`), and builds iPXE trusting only that CA
   (`task ipxe:build:testca`).
2. Creates network `heph-e2e` (`10.99.0.0/24`), builds the fixture image (`fixture/`, see Pins) and copies its `/fixture` to
   `build/e2e/run/upstream`, and starts the fake upstream: a Caddy container `heph-e2e-upstream` at `.12`, TLS 1.3 only
   (`upstream.json`), serving that directory with a certificate for `upstream.e2e.test`. It then starts the upstreams program
   (`upstreams/`), `heph-e2e-upstreams` at `.13`, with `go run` from the working tree in the pinned Go image (which carries
   git). It is one TLS 1.3 listener for three names, each with its own leaf, picked by SNI:
   - `factory.e2e.test`: the fake image factory (`internal/factory/factorytest`) for the plain schematic and Talos
     `v1.12.11`, amd64. It answers each image request with a 302 to `assets.factory.e2e.test`, as `factory.talos.dev` sends
     clients to `assets.factory.talos.dev`, so hephaestus must follow a redirect to another host. Its kernel is an iPXE
     script that prints `HEPHAESTUS-E2E-TALOS-KERNEL booted` and sleeps (iPXE's `kernel` refuses an image it does not
     recognise, which would end the install script before its `initrd` line); the initramfs and command line are a few
     bytes each.
   - `assets.factory.e2e.test`: the asset URLs, with Range and a strong ETag; the factory host does not serve them.
   - `git.e2e.test`: git smart HTTP (`internal/clusters/gittest.Handler`, the host git's `http-backend`) over `lab.git`, which
     the program commits at start from `fixture-cluster/` (the Topf tree `lab/` with the plain schematic, the fixture
     references and their `secrets.yaml`; `values.json` holds the values those references resolve to).

   It is configured only by `UPSTREAMS_ADDR`, `UPSTREAMS_{FACTORY,ASSETS,GIT}_{CERT,KEY}_FILE`, `UPSTREAMS_FIXTURE_DIR` and
   `UPSTREAMS_WORK_DIR` (a tmpfs), all required, and writes one JSON `request` line per request (host, protocol, method, path without
   the query, status) to stdout. Its `validate-events` subcommand checks the run's event record (see Output and cleanup). Its
   unit tests (`upstreams/main_test.go`) run in `task ci`.
3. Copies the tracked tree to `build/e2e/run/tree` (not a `mktemp` directory: the pin container bind-mounts it), copies
   `overlay/catalog.go.in` over its `internal/catalog/catalog.go` and overlays `internal/boot/ipxe/bin/` with the test-CA binaries
   and `roots.pem`. The overlay (four specs, all version `1`, trust on first use) names `e2e-shell` (a zip holding the UEFI Shell,
   chained on UEFI) and `e2e-live` (an ISO on amd64, a zip on arm64, each with a kernel, an initrd and a squashfs) at
   `https://upstream.e2e.test/`. The working-tree copy of the overlay is used, because the tree copy holds only committed files.
4. Builds the real pin tool (`cmd/catalogpin/Dockerfile`) from that tree as `hephaestus-catalogpin:e2e` and runs it on the e2e
   network with `--add-host upstream.e2e.test:10.99.0.12` and `SSL_CERT_FILE=/certs/ca.pem`. It downloads the objects over TLS
   1.3, extracts the files and writes `catalog_pins.go` into the tree: the hashes are generated, never hand-written.
5. Builds `hephaestus:e2e` with the unmodified production `Dockerfile` from that tree, then deletes the tree, so no test-CA binary
   or overlay reaches the repository tree or the production image. Before the build it copies two more overlays into the tree:
   `overlay/base.go.in` over `internal/factory/base.go` (`const Base = "https://factory.e2e.test"`) and `overlay/resolver.go.in`
   over `internal/secrets/onepassword.go`, a resolver with the 1Password resolver's exported API that answers from
   `fixture-cluster/values.json`, copied beside it as `internal/secrets/e2e_values.json` for its `//go:embed` (embed paths
   are package-relative). The `.go.in` files are never compiled in the repository, so the production binary keeps the
   public factory and the real 1Password SDK.
6. Starts dnsmasq at `.2` (DHCP and DNS, no PXE options), hephaestus at `.10` (file mode; the e2e CA and the `upstream.e2e.test`
   host entry are always present, so it reaches the upstream by name and SNI matches), the QEMU toolbox at `.20`, and in proxy
   mode Caddy at `.11` (`caddy.json`). The VM's tap is bridged to the toolbox's `eth0`, which puts it on the same broadcast domain
   as dnsmasq and hephaestus. hephaestus also gets host entries for the three upstreams names, `HEPHAESTUS_DATA_KEY` set to the
   test key (`base64(32 × 0x01)`) and `HEPHAESTUS_OP_TOKEN=e2e-dummy`, which only satisfies the rule that a token is set (the
   fixture resolver ignores it).
7. Sets `unknown_action` to `hold` with `hephaestus unknown-action set hold --yes`, then boots, one at a time, each with a 120 s
   timeout, stopping at the first hold: `ovmf` (amd64 UEFI), `seabios` (amd64 BIOS), `aarch64` (arm64 UEFI), all in file mode.
8. The utility phase (M2) uses those three machines, which hephaestus has now seen. It asks `serve` to fetch both entries with
   `docker exec heph-e2e-hephaestus /hephaestus utility fetch`, polls up to 120 times for `utility list --json` to show four `ready`
   rows, then runs three cases, each VM with `boot-next set` first and a 300 s timeout (`UTILITY_TIMEOUT`):
   1. EFI chain: `e2e-shell` on `ovmf` and `aarch64`.
   2. Kernel, initrd and squashfs: `e2e-live` on `ovmf`, `seabios` and `aarch64`.
   3. Unsupported: `e2e-shell` set on the `seabios` VM. The entry has an amd64 build, so the CLI accepts it, but it lacks `pcbios`.
9. The install phase (M3), on the `ovmf` machine (`52:54:00:e2:e0:03`), which M1 left `discovered`:
   1. `cluster import lab --repo https://git.e2e.test/lab.git --ref main --path lab`; polls up to 120 times for `cluster list
      --json` to show a pending pair for `lab` (a new cluster is fetched at once); `cluster approve lab <pair id>`; `machine
      approve 52:54:00:e2:e0:03 lab/cp-01`.
   2. Boots `ovmf` with a 300 s timeout (`INSTALL_TIMEOUT`) until the serial console shows `HEPHAESTUS-E2E-TALOS-KERNEL
      booted`. The first `/boot` may hold at row 8 while hephaestus fetches the Talos assets; the hold script asks again
      every 30 s, and row 10 answers the install script.
   3. Stands in for Talos: Talos's own HTTPS `/config` fetch cannot trust the test CA (it stays lab test 3), so the harness
      asks `/boot` as the machine from a container on the subnet, as iPXE does (its UUID, MAC, serial and product), takes the
      `talos.config` URL from the install script and fetches it twice with curl, trusting only the test CA. The script and the
      config hold the ticket and the fixture's secret values, so neither is written to the output directory.
10. Boots `ovmf` again in proxy mode. Each VM has its own MAC and `-uuid`.

## Checks

Per boot: the VM reached its hold; dnsmasq leased it an address; proxyDHCP offered the expected boot file and TFTP sent it;
`/start` was 200 over plain HTTP; `/boot` was 200 with decision row 7 and action `hold`, over TLS; `POST /discover` was 200
`stored`; no `TLS handshake error` line; on UEFI, the serial console shows the discovery report completing and
`hephaestus: holding: awaiting approval`; SQLite has exactly one `discovered` row for the MAC with `junk=0`, the right
architecture and the VM's UUID; `hephaestus_boot_decisions_total{action="hold",decision_row="7"}` rose. SeaBIOS and its iPXE ROM
write to VGA only, so the BIOS boot is judged from the server log. In proxy mode Caddy's access log must also show the `/boot`
request as TLS 1.3, and hephaestus sees the client in `client_ip`.

Utility setup: the pin tool's summary is `PINNED 4 ABSENT 0` (the run stops there otherwise); `utility fetch e2e-shell` and
`utility fetch e2e-live` exit 0; within 120 polls all four rows are `ready`; the hephaestus log has four `utility fetch` lines with
`"to":"ready"`.

Per utility boot: `boot-next set` exits 0; the evidence below was reached; `/boot` was 200 with decision row 3, action `utility`,
the entry, and `boot_next_clear` `served`; every file the recipe loads (`/u/<entry>/<arch>/1/<path>`) was 200 from the VM's address
over plain HTTP (`/u/` is plain HTTP by design); `hephaestus_utility_boots_total{entry=…}` rose; the machine's `boot_next` is NULL
and exactly one more `boot_next.cleared` audit event with the expected `reason` exists. The evidence is judged by what the firmware
can show:
- Case 1 (`chain`) needs `UEFI Interactive Shell` on the serial console.
- Case 2 (`kernel`, `initrd`, `boot`): on UEFI, the serial line `HEPHAESTUS-E2E-SECOND-STAGE ok bootif=01-<MAC with hyphens>
  sha256=<hash>`, where the hash is `fixture/squashfs.sha256` (the initrd fetched the squashfs by hephaestus's IP, using the
  `BOOTIF` iPXE expanded, and hashed it). On BIOS the firmware writes to VGA only and the poll stops the VM at the squashfs
  request, so that request in the server log is the evidence.
- Case 3: the VM held, as in M1's BIOS boot: `/boot` was 200 with decision row 7, action `hold` and `boot_next_clear`
  `unsupported`, and the hold script's `POST /discover` was 200 (any outcome); no `/u/` request was made; the hold counter rose and the
  utility boots counter did not; the audit `reason` is `unsupported`.

Serial lines end in `\r\n`, so the harness strips `\r` and matches each marker as a substring (kernel messages can share the UART).
A `HEPHAESTUS-E2E-SECOND-STAGE fail <reason>` line means the kernel and initrd ran, so it is a fixture or harness failure and
the exact-line check fails; a `/u/` 200 for the `.efi` with no `UEFI Interactive Shell` would be a failure of the `chain`
mechanism, and a missing second-stage evidence after the kernel and initrd were fetched one of `kernel`+`initrd`+`boot`.

Install phase: `cluster import`, `cluster approve` and `machine approve` exit 0 and the pending pair appears within 120 polls;
the VM reached `HEPHAESTUS-E2E-TALOS-KERNEL booted` on serial; dnsmasq leased it an address; `/boot` was 200 with decision row 10
and action `install` over TLS from the VM; `GET /t/<schematic>/v1.12.11/amd64/kernel` and `/initramfs` were 200 from the VM over
plain HTTP; `machine show --json` reports the machine `assigned` to `lab/cp-01`, its Talos row `ready` and a live ticket whose
config was not asked for yet. Then, for the harness's own fetch: the install script carries a `talos.config` URL with a ticket;
`/config` with it serves a control-plane machine config and the same ticket again gets 404; hephaestus logged exactly one
`/config` 200 `served` and one 404 `refused_*`; the ticket appears nowhere in its log, in `events.jsonl` or in `run.log` so far;
`machine show` reports `installing` with no live ticket and the audit log holds one `config.served` event. In the upstreams log:
`lab.git` was fetched over smart HTTP; each of `kernel-amd64`, `initramfs-amd64.xz` and `cmdline-metal-amd64` was answered 302 on
`factory.e2e.test`; and `assets.factory.e2e.test` served at least three assets, so hephaestus followed the cross-host redirects.
No value from `fixture-cluster/values.json` appears in the hephaestus or the upstreams log, in `events.jsonl` or in `run.log` so
far. Each upstreams name completes a TLS 1.3 handshake that verifies for that name against the test CA, and TLS 1.2 gets a
protocol-version alert.

Cleanup: `events.jsonl` matches `events.schema.json` (see Output and cleanup).

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
dump, the pin report and the generated `catalog_pins.go`, saved as `catalog_pins.go.txt` so that lint does not typecheck it, and
three files that record the run:

- `events.jsonl`, the run's record: one JSON object per line, in the order emitted. Each has `schema`
  (`hephaestus.e2e.event/v1`), `ts` (RFC 3339, UTC, seconds), `seq` (from 1) and `type`. A run is `run_start` (`run_id`,
  `out_dir`, `git_commit`, `mode`), then per phase a `phase_start` (`phase`, a stable id; `title`; `labels`), a `check` per
  check (`phase`, `name`, `result` `pass`, `fail` or `note`, an optional `detail` with what was got, `evidence` naming the
  captured files it read, relative to the run directory, and `elapsed_s`) and a `phase_end` (its counts and `duration_s`),
  then `run_end` (`result`, the totals, `duration_s`, `failures` as `{phase, name}`, `exit_code`). `events.schema.json` (JSON
  Schema 2020-12) defines every field. At the end of every run the cleanup phase validates the record against it with the
  upstreams program's `validate-events`, in the golang container, and a mismatch is a `FAIL`; the schema and the emitter
  (`lib/events.sh`, driven by `lib/events_test.sh`) have unit tests in `upstreams/main_test.go`, which run in `task ci`.
  No secret, ticket, rendered config or fixture value goes into an event, and the install phase checks that neither the
  ticket nor any value of `fixture-cluster/values.json` appears in `events.jsonl` or `run.log`.
- `run.log`: everything the run printed to stdout and stderr, every docker build and pull included, as the console showed
  it, without colour codes. `run.sh` runs itself again under a capture that copies the stream with `tee` and hands the run's
  exit status back to the wrapper, which a plain `| tee` would lose (POSIX `sh` has no `pipefail`).
- `manifest.json`: every other file in the directory with its `path`, `size` and `sha256`, and `run_id` and `git_commit`,
  written once `run.log` is complete, in the toolbox container (the host is not assumed to have a sha256 tool). A run
  whose manifest cannot be written exits non-zero.

The phases, in order: `leftovers`, `toolbox`, `certs`, `ipxe`, `network`, `upstream`, `upstreams`, `tree`, `pin`, `image`,
`start-file`, `run-level-file`, `boot-ovmf`, `boot-seabios`, `boot-aarch64`, `utility-fetch`, `utility-case-1`,
`utility-shell-ovmf`, `utility-shell-aarch64`, `utility-case-2`, `utility-live-ovmf`, `utility-live-seabios`,
`utility-live-aarch64`, `utility-case-3`, `utility-unsupported-seabios`, `install`, `install-boot`, `config-fetch`,
`upstreams-log`, `proxy`, `boot-ovmf-proxy`, `sqlite`, `cleanup`. A setup phase (`toolbox`, `ipxe`, …) has no checks; a
phase the run stopped in ends where the cleanup starts.

A run's failures, with what each got and where to look:

```sh
jq -c 'select(.type == "check" and .result == "fail") | {phase, name, detail, evidence}' build/e2e/<UTC timestamp>/events.jsonl
```

The run exits non-zero on any failure and zero only when every check passed. A trap on exit, SIGINT and SIGTERM removes every
`heph-e2e-*` container, the network, the volume, `build/e2e/run/tree`, `build/e2e/run/upstream` and the certificates, and the
test-CA build output under `build/ipxe/out` only if this run created it (it never deletes files that were already there). On
Ctrl-C the capture ignores the signal and waits for that trap, so the cleanup and the summary still reach the terminal and
`run.log`, and the manifest is still written. If a run was killed with SIGKILL, the next run removes the containers, network,
volume, tree and upstream directory it left; the git-ignored files it left under `build/ipxe/out` are overwritten by the next
run.

## Console

The console shows the same events as `events.jsonl`, written by the same calls (`lib/events.sh`), so the two cannot
disagree. A shortened run with two failures:

```text
━━ hephaestus e2e · run 20261121T040506Z · file+proxy mode ━━━━━━━━━━━━━

▸ Boot ovmf  amd64 · UEFI · file mode
  PASS  held within 120s (after 18s)
  PASS  dnsmasq leased an address to 52:54:00:e2:e0:03 (10.99.0.131)
  PASS  proxyDHCP offered snp.efi to 52:54:00:e2:e0:03
  PASS  /boot 200 row 7 hold (tls:true at hephaestus)
  ── 4 passed · 0 failed · 21s

▸ Boot seabios  amd64 · BIOS · file mode
  PASS  held within 120s (after 11s)
  NOTE  serial not asserted (SeaBIOS writes to VGA only); the server log above is the evidence
  ── 1 passed · 0 failed · 1 note · 14s

▸ Config fetch install-ovmf (the harness in Talos's place)  52:54:00:e2:e0:03
  PASS  GET /config with the ticket is served a control-plane machine config
  FAIL  the same ticket again gets 404
        got:      200
  FAIL  hephaestus logged exactly one /config 404 refusal
        got:      0
        evidence: install-ovmf.hephaestus.log
  ── 1 passed · 2 failed · 7s

━━ Summary ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
  phase         pass  fail    time
  boot-ovmf        4     0     21s
  boot-seabios     1     0     14s
  config-fetch     1     2      7s

  RESULT  FAIL   6 passed · 2 failed · 1 note · 42s
  FAIL  config-fetch · the same ticket again gets 404
  FAIL  config-fetch · hephaestus logged exactly one /config 404 refusal

  record:       build/e2e/20261121T040506Z/events.jsonl
  full output:  build/e2e/20261121T040506Z/run.log
  manifest:     build/e2e/20261121T040506Z/manifest.json
```

A header names the run. Each phase starts after a blank line with `▸` and its title, its labels dimmed beside it; each check is
indented with `PASS`, `FAIL` or `NOTE` in a fixed column (a pass or note shows its detail in parentheses); a failure shows what
it got and its evidence files under it; and a `──` subtotal closes the phase. A failed boot also prints the tail of its serial
console (`serial|`) under the failure, and a step that stops the run, such as a failed image build, prints its build log's
tail; when no check failed but the run stopped, the summary says so. The summary has a row per phase, the result with the
totals and the run's duration, every failure again, and where the record, the full output and the manifest are. Durations read
`14s`, `5m12s` or `1h02m`. Colour is used only when stdout is a terminal and `NO_COLOR` is unset (https://no-color.org), and
the words `PASS` and `FAIL` stay, so `grep FAIL build/e2e/<UTC timestamp>/run.log` works.

## Pins

The toolbox, the iPXE builder, the fixture image and the pin tool's runtime start from `debian:trixie-slim` by digest and install
packages from the dated Debian snapshot `20261104T212339Z`. The upstreams program runs in `golang:1.27.4-trixie`, pinned by digest in `run.sh` (the image the
production `Dockerfile` builds with; it ships git 2.47.3). Debian trixie's OVMF has no network boot stack, so the amd64 UEFI
firmware is Ubuntu noble's `ovmf_2024.02-2ubuntu0.10_all.deb`, checked by sha256 (`qemu/Dockerfile`). arm64 uses Debian's AAVMF,
which does PXE. Caddy (the proxy and the fake upstream) is pinned by digest in `run.sh`.

The fixture image (`fixture/Dockerfile`) is built once and cached: edk2's `ShellBinPkg.zip` (checked by sha256), and from the
snapshot's stock Debian kernels and `busybox-static` for amd64 and arm64, an ISO and a zip each holding a kernel, an initrd and a
1 MiB stand-in squashfs. The initrd's `/init` (`fixture/init`) brings up the NIC, fetches the squashfs named by `fetch=` and prints
its `HEPHAESTUS-E2E-SECOND-STAGE` line, then powers off (QEMU runs with `-no-reboot`). The pin tool pins these files by their hashes
at run time, so nothing about them is written into the repository.
