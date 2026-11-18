# M1 lab bring-up: TrueNAS + one amd64 node + the MS-R1

This is a task guide to the lab checks that `task e2e` cannot cover (design §11):

- **Lab test 1 (rest):** iPXE 9261ebe validating a real ISRG-rooted chain with internet egress blocked.
- **Lab test 2:** `${proxydhcp/next-server}` on physical NIC firmware and on the MS-R1 shim path.
- **Lab test 7 (rest):** TrueNAS's real container defaults (`no-new-privileges`, capabilities, the unprivileged-port floor), the dedicated VLAN interface, the GUI restricted to the management IP.

File mode is used throughout. Proxy mode is optional (step 9). Fill in every `<…>` placeholder, then paste the "Results" table and any failing log lines back into the session.

Time: about 60 minutes once the certificate exists.

## 0. Preconditions (check before you start)

| # | Check | Command or place | Expected |
|---|---|---|---|
| 0.1 | TrueNAS architecture | TrueNAS shell: `uname -m` | `x86_64` (selects the image platform in step 1) |
| 0.2 | Dedicated VLAN interface, with hephaestus's IP as its **primary** address | `ip -4 addr show <vlan-iface>` | first `inet` is `<prov-ip>/<len>` |
| 0.3 | Nothing else on the boot ports | `ss -lntup \| grep -E ':(67\|69\|80\|443\|4011)\b'` | no output; no other proxyDHCP, and booty dormant (design §3) |
| 0.4 | TrueNAS web UI bound to the management IP only (design §5.11) | System → General → GUI → Web Interface IPv4 Address | `<mgmt-ip>`, not `0.0.0.0`; then 0.3 again |
| 0.5 | LAN DNS: `<fqdn>` → `<prov-ip>` | from a VLAN host: `dig +short <fqdn>` | `<prov-ip>` |
| 0.6 | The VLAN's normal DHCP server still hands out addresses | gateway or UniFi | a lease range on the VLAN (hephaestus is proxyDHCP only) |

## 1. Certificate (outside hephaestus)

hephaestus never obtains a certificate (D25). Get one for `<fqdn>` from Let's Encrypt with certbot or lego (DNS-01 suits a private VLAN name), then put it on TrueNAS at `<cert-dir>`.

- `grep -c 'BEGIN CERTIFICATE' <cert-dir>/fullchain.pem` → **2 or more**. A leaf-only `cert.pem` gives 1, and hephaestus refuses it (D27).
- `openssl x509 -in <cert-dir>/fullchain.pem -noout -subject -issuer -enddate` → the subject or SAN covers `<fqdn>`, the issuer is a Let's Encrypt intermediate, and the end date is in the future.
- The files must be readable by UID 65532, e.g. `chown -R 65532:65532 <cert-dir>`, or group-readable with the container's group added.
- Renewal: the tool rewrites the files in place or by symlink. hephaestus re-reads them every minute, and keeps serving the old pair if the new one fails its checks.

## 2. Image onto TrueNAS (no registry yet)

On the Mac, in the repo:

```sh
export DOCKER_HOST=unix://$HOME/.colima/default/docker.sock
task image:build PLATFORM=linux/amd64          # amd64 builds are untested so far; report anything odd
docker run --rm --platform linux/amd64 hephaestus:dev version --json   # commit = git rev-parse --short HEAD
docker save hephaestus:dev | ssh <truenas> docker load
```

On TrueNAS: `docker image inspect hephaestus:dev --format '{{.Config.User}} {{.Architecture}}'` → `65532:65532 amd64`.

## 3. TrueNAS pre-flight (lab test 7)

Record each answer in Results; none of them is a pass/fail gate yet.

```sh
docker info --format '{{json .SecurityOptions}}'      # does the daemon default to no-new-privileges?
sysctl net.ipv4.ip_unprivileged_port_start            # host netns (host networking): 1024 means the capability is exercised
```

## 4. The app

TrueNAS 24.10+ runs custom apps from Compose YAML. `user`, `cap_drop`/`cap_add`, the network mode and the stop grace are the parts that matter. `stop_grace_period` is set above `HEPHAESTUS_SHUTDOWN_GRACE` (60 s).

```yaml
services:
  hephaestus:
    image: hephaestus:dev
    pull_policy: never
    network_mode: host
    user: "65532:65532"
    cap_drop: [ALL]
    cap_add: [NET_BIND_SERVICE]
    stop_grace_period: 70s
    restart: unless-stopped
    volumes:
      - hephaestus-data:/var/lib/hephaestus
      - <cert-dir>:/certs:ro
    environment:
      HEPHAESTUS_FQDN: <fqdn>
      HEPHAESTUS_PROVISIONING_INTERFACE: <vlan-iface>
      HEPHAESTUS_PROVISIONING_ADDRESS: <prov-ip>
      HEPHAESTUS_PROVISIONING_SUBNET: <prov-cidr>
      HEPHAESTUS_TLS_CERT_FILE: /certs/fullchain.pem
      HEPHAESTUS_TLS_KEY_FILE: /certs/privkey.pem
      # HEPHAESTUS_ADMIN_ADDRESS defaults to 127.0.0.1:9090 (TrueNAS loopback); set <mgmt-ip>:9090 to reach it from the LAN
volumes:
  hephaestus-data: {}
```

## 5. Start and readiness

```sh
docker logs -f <container> 2>&1 | jq -c '{time,level,msg,path,status,tls,decision_row,action,error}'
```

| Expect | Where |
|---|---|
| One startup Info line with `tls_mode":"file"`, `fqdn`, `address`, `subnet` | log |
| No `certificate chain does not reach a root…`, no `bind: permission denied` | log |
| `curl -s http://127.0.0.1:9090/readyz` → `ok` | TrueNAS shell |
| `curl -s http://127.0.0.1:9090/health \| jq '.checks'` → `tls:certificate` pass, `tls:expiry` pass (warn if under 14 days) | TrueNAS shell |
| `ss -lntup \| grep hephaestus` → UDP 67, 69 and 4011, and TCP 80 and 443 on `<prov-ip>`/`<vlan-iface>`; 9090 on the admin address | TrueNAS shell |
| `grep Cap /proc/$(pgrep -f '/hephaestus serve')/status` → note `CapEff` | TrueNAS shell |

A `permission denied` on a bind means the capability did not take effect: TrueNAS dropped it, or `no-new-privileges` blocked the file capability. Record which. Design §10's documented fallback is running as root.

## 6. Let unknown machines hold, not boot local disk (only for the test)

The default `unknown_action=local` sends unknown machines to their own disk (row 7 `exit`) without a `/discover`. M1 has no UI, so switch it in the database. **Every** unknown PXE-booting machine on the VLAN then holds until you switch it back (step 10).

```sh
docker run --rm --user 65532:65532 -v hephaestus-data:/d alpine:3 sh -c \
  "apk add -q sqlite && sqlite3 -cmd '.timeout 5000' /d/hephaestus.db \"UPDATE settings SET value='hold' WHERE key='unknown_action'; SELECT value FROM settings WHERE key='unknown_action';\""
```

Expected output: `hold`. The unpinned `alpine:3` plus `apk` is acceptable for a one-off lab command.

## 7. Boot A: an amd64 node, UEFI network boot (lab tests 1 and 2)

1. **Block internet egress for the VLAN** at the gateway for the length of the boot. The ISRG chain must validate from what hephaestus sends plus the roots built into iPXE.
2. Network-boot the node and watch the log from step 5. Expected, in order:
   - `offering boot response` (proxyDHCP)
   - `file sent` with filename `snp.efi` (TFTP; a BIOS node gets `undionly.kpxe`)
   - `request /start 200 tls:false`
   - `request /boot 200 tls:true decision_row:7 action:hold`
   - `request /discover 200` (stored)
   - **no** `TLS handshake error`
3. On screen: iPXE prints `https://<fqdn>/discover... ok`, then `hephaestus: holding: awaiting approval`.
4. Machine row:

   ```sh
   docker run --rm --user 65532:65532 -v hephaestus-data:/d alpine:3 sh -c "apk add -q sqlite && sqlite3 /d/hephaestus.db \"SELECT m.state, m.junk, m.arch, m.smbios_uuid, mm.mac FROM machines m JOIN machine_macs mm ON mm.machine_id = m.id;\""
   ```

   Expected: `discovered|0|amd64|…` for its MAC.

## 8. Boot B: the MS-R1 through its shim media (lab test 2)

The shim media carries hephaestus's static arm64 `snp.efi`, from `http://<prov-ip>/tftp/arm64/snp.efi` (design §10, "MS-R1 shim"). Boot it with egress still blocked. Expected:

- `request /tftp/arm64/snp.efi 200`, if the shim fetches it;
- `/start`, `/boot` (`tls:true`, hold) and `/discover`, as in step 7;
- the row's `arch` is `arm64`.

**Record exactly what the console shows** around DHCP. The open question is whether `${proxydhcp/next-server}` is populated on this firmware path, because the embedded script uses it. If `/start` never arrives, photograph the iPXE screen.

## 9. Optional: proxy mode

Only if you plan to front hephaestus with Caddy, Traefik or Envoy:

- Follow README "TLS → proxy", including `apps.tls.session_tickets.disabled` for Caddy (D26).
- Envoy needs `use_remote_address: true`.
- Repeat step 7. The `/boot` line then shows `tls:false` at hephaestus, with a `client_ip`.

## 10. Restore

```sh
docker run --rm --user 65532:65532 -v hephaestus-data:/d alpine:3 sh -c "apk add -q sqlite && sqlite3 -cmd '.timeout 5000' /d/hephaestus.db \"UPDATE settings SET value='local' WHERE key='unknown_action';\""
```

Unblock egress. Leave the app running or stop it; booty stays dormant either way.

## Results (paste back)

| Item | Result | Evidence (one line) |
|---|---|---|
| 0.1 arch | | |
| 0.3/0.4 ports free after the GUI move | | |
| 3 `no-new-privileges` default | | |
| 3 `ip_unprivileged_port_start` | | |
| 5 starts as 65532 and binds all ports | | |
| 5 `CapEff` | | |
| 5 `/readyz` ok, `tls:*` pass | | |
| 7 amd64: proxyDHCP → TFTP → `/start` → `/boot` (TLS, egress blocked) → `/discover` → hold | | |
| 7 zero `TLS handshake error` lines | | |
| 8 MS-R1: `${proxydhcp/next-server}` populated | | |
| 8 MS-R1: reaches hold, arch `arm64` | | |
| 9 proxy mode (optional) | | |

[← Back to README](../../README.md)
