# Install Talos

This is the task guide for installing Talos on a machine with hephaestus: import a Topf cluster definition from git, approve what it renders, approve a machine into a host, and confirm the install. It also holds the reinstall steps and the runbooks for a failed install.

Every command runs inside the container, for example `docker exec hephaestus /hephaestus cluster list`, and writes only the database. `serve` does every fetch, 1Password call and render, and picks a request up within 10 seconds. The full command list is in [Admin CLI](admin-cli.md).

A **pair** is the git tree of the cluster's directory together with the secret values resolved for it. You approve a pair, not a commit: a commit outside the directory creates nothing, and a changed secret value creates a new pending pair for the same tree.

## Before You Start

- `serve` runs with a data key ([Data Key](configuration.md#data-key)). A definition that uses `ref+op://` references also needs a 1Password service account token ([1Password Token](configuration.md#1password-token)).
- The definition is a Topf directory in an https git repository that meets [What the Definition Must Satisfy](#what-the-definition-must-satisfy).
- `serve` can reach the git host and `factory.talos.dev` over TLS 1.3.
- The node resolves `HEPHAESTUS_FQDN`, has a correct clock and trusts the boot certificate: Talos fetches its config from `https://<HEPHAESTUS_FQDN>/config`.
- The machine has PXE-booted through hephaestus once, so it shows in `machine list` as `discovered`.

## Import and Approve the Cluster

1. Import the cluster. `--ref` is a branch to follow, not a tag. A private repository takes `--username` and one `ref+op://` reference to its password or token; a URL with a user or password in it is refused.

   ```sh
   docker exec hephaestus /hephaestus cluster import lab \
     --repo https://git.example.com/lab/cluster.git --ref main --path clusters/lab
   ```

2. Wait for the first pair. `serve` fetches a new cluster at once and every 5 minutes after that, and refreshes its secret values daily.

   ```sh
   docker exec hephaestus /hephaestus cluster list
   ```

   The pending pair id shows when the render succeeded. A tree that failed shows as the failed tree; `cluster list --errors` prints why, and that text can quote a fragment of a secret, so keep it out of tickets and logs.

3. Read what it renders, then approve it by id.

   ```sh
   docker exec hephaestus /hephaestus cluster diff lab
   docker exec hephaestus /hephaestus cluster approve lab 7QF3K2X5HDXBBA5YZLVC3AMMTK
   ```

   The diff is per host and redacted; a host whose real config changed but whose redacted render did not shows "secret values changed", and the references the pair resolved are listed so you can see which items fill the redacted values. A newer render replaces the pending pair and makes its id stale: run the diff again.

Approving replaces the older approved pair, revokes every live ticket of the cluster, and orphans any bound machine whose host the new definition lacks.

## Install a Machine

1. Approve the machine into a host of the approved pair by its SMBIOS UUID (`machine list` shows it). The host must be unbound and the UUID must not be junk. Approving by MAC is refused for 24 hours after the MAC changes owner.

   ```sh
   docker exec hephaestus /hephaestus machine approve 4c4c4544-0042-3510-8052-b4c04f4e4d32 lab/cp-01
   ```

   The command prints the machine's UUID, serial and product before it binds; check them against the machine you mean.

2. Wait for its files. `serve` fetches the Talos kernel, initramfs and kernel command line for the host's schematic, version and architecture; `machine show` reports their state. Until they are `ready` the machine holds and re-checks every 30 seconds. See [Talos Boot Assets](#talos-boot-assets).

3. Boot the machine. Its install script loads the kernel and initramfs over plain HTTP from `/t/` and passes the kernel command line, `talos.halt_if_installed=1` and `talos.config=https://<fqdn>/config?u=<uuid>&t=<ticket>`. The ticket is single use and valid for 3 hours.

4. Talos fetches `/config` once. The machine moves to `installing`. When the node reboots, its next network boot exits to its disk and the machine becomes `install-unconfirmed`.

5. Confirm it once Talos is up.

   ```sh
   docker exec hephaestus /hephaestus machine mark-installed 52:54:00:12:34:56
   ```

hephaestus stops there: bootstrapping etcd and fetching the talosconfig and kubeconfig stay with Topf. A machine that already runs Talos is bound without an install by `machine adopt <mac|uuid> lab/cp-01`, or `machine adopt --uuid <uuid> --arch <amd64|arm64> lab/cp-01` when it has never booted through hephaestus.

## What the Definition Must Satisfy

A tree that breaks a rule is refused with an error that names the file and the rule, and shows as the failed tree. The render is Topf's, vendored at a pinned commit and proven byte-identical to `topf render`, so a definition that renders there renders here unless it breaks one of these.

- `topf.yaml` names a `talosVersion` that is a release from v1.12.11 to v1.14.x (a pre-release is refused, and a per-node version must match) and a `kubernetesVersion`. The platform is `metal`, the factory is the default, and `secureboot` is off.
- A `secrets.yaml` bundle exists and parses. hephaestus never generates one.
- Secrets are `ref+op://VAULT/ITEM/[SECTION/]FIELD` references, each a whole string scalar, with no `?` query and no `#` fragment. Any other `ref+` scheme, a reference inside a longer string, and SOPS-encrypted files are refused.
- `nodesProvider`, `secretsProvider` and `configDir` are refused, because they would run a program or read outside the directory.
- Host names are unique, lowercase DNS names.
- No path is absolute or contains `..`, no symlink leaves the tree, and the tree stays under 20,000 files and 128 MiB.
- Templates use a fixed list of functions: nothing that depends on time, randomness or map order, no key generators, no `keys` or `values`, and no `.PatchesDir`. A value in `data` must be a plain string, number, boolean, list or map.
- A schematic file must not contain a secret: it is rendered twice with different values, and a difference refuses the tree, because the body is sent to the public image factory.

## Refresh and Drift

A refresh resolves the references again. The daily timer does it, and so does `cluster refresh lab`. If any value changed, the result is a new pending pair to review; a failed reference keeps the approved value and marks the refresh incomplete (`secrets:cache` warns and the `refresh_incomplete` alert is raised). A reference that failed is retried by the next `cluster refresh`, not on a timer.

Drift is an approved pair whose render no longer hashes to what was approved. The host holds, `drift` is alerted, and no config is served for it. Look, then approve what you saw:

```sh
docker exec hephaestus /hephaestus cluster diff lab --host cp-01
docker exec hephaestus /hephaestus cluster approve lab --host cp-01 --hash 9f86d081884c7d65
```

## Reinstall

The install script always sets `talos.halt_if_installed=1`, and Talos halts when its META partition exists, so a machine must be wiped before it installs again. In this order:

1. `topf reset` wipes the whole disk (`--full` defaults to true) and reboots the node. While the machine is still `installed` hephaestus answers its boot with `exit`, and it finds no OS.
2. `machine reinstall <mac|uuid> --topf-reset-full-done` says you did it. The command is refused without the flag.
3. Power-cycle the node, or wait for its next network boot.

`topf reset --full=false` keeps META and leads to a halt at every boot. Running `reinstall` first is accepted but wrong: any reboot before the wipe would halt. `machine abort` carries the same warning: if the installer already wrote META, abort needs a full reset before the next install.

## Talos Boot Assets

Talos boot files come from the public image factory, through the same worker pool as the utility downloads (`HEPHAESTUS_FETCH_WORKERS`). A row exists per schematic, version and architecture, with the states `wanted`, `fetching`, `ready`, `failed` and `broken`, retried with the same backoff as a utility: from one minute, doubling to one hour, and the tenth failure is `broken`.

- When a row has a schematic file, `serve` registers it with the factory first and compares the id the factory returns; a mismatch is `broken`. A schematic given as a 64-hex id is not registered.
- Each file is downloaded from `https://factory.talos.dev/image/<schematic>/<version>/`, which redirects to a signed `assets.factory.talos.dev` URL. One tuple is about 100 MB. Space is reserved as for utilities ([Free Space](utility-content.md#free-space)), and a file over 1 GiB is refused.
- The factory publishes no hash, so each file's size and sha256 are recorded at its first complete fetch and are the authority afterwards. A later difference is an integrity failure and the row is `broken`. `machine fetch <mac|uuid> --reset-hash` clears them and downloads afresh: that is a trust decision, and it is audited.
- A resumed download is accepted only when the upstream ETag still matches, so a changed file restarts from zero.
- The kernel command line is checked: printable ASCII, no `$`, no `;`, `&&` or `||`, no token starting with `#`, and at most 2046 bytes once the install adds the halt flag and config URL. A line that cannot fit holds the machine and raises `cmdline_too_long`; shorten the schematic's `extraKernelArgs`.
- At startup a `fetching` row goes back to `wanted`, and a `ready` row whose content is missing or differs from the recorded hashes does too. Talos tags are never garbage-collected in this release.

## When Something Goes Wrong

Alerts show in `audit list`; the checks are in [Observability](observability.md).

**The machine holds after approval.** Its files are not ready. `machine show <mac|uuid>` gives the row's state and last error. A `failed` row retries by itself; a `broken` row, which `talos:cache` reports, needs `machine fetch <mac|uuid>`, or `--reset-hash` if you accept a changed upstream build. A render that fails or drift also holds the host: see `render:approved` and the `drift` alert.

**`config:pending` warns for a ticket nobody asked for.** The node never fetched `/config`: check its DNS for `HEPHAESTUS_FQDN`, its clock and its trust of the boot certificate. `machine show` gives the ticket's minted time and whether `/config` was ever asked for.

**A ticket was asked for and is still live.** The request was refused or the delivery was not recorded. `audit list --machine <mac|uuid>` shows a `config.refused` event with the reason (a stale pair, drift, a ticket for another machine, an off-subnet source).

**Alert `config_unrecorded`: config delivered but not recorded.** The node has its config and the database could not record it. By the machine's state:

- still `assigned`: `machine unassign`, then `machine adopt <mac|uuid> <cluster>/<host>`;
- `cordoned` in that instant: `machine uncordon` first, then as above;
- `discovered`: `machine adopt` only;
- `orphaned` (its host left in a racing approval): `machine unbind`. The node runs a config for a host that no longer exists, so it needs `topf reset` and an approval into a current host.

**Recorded but not received.** The machine is `installing` but the node never got its config (the connection dropped after the write). `machine abort`, then reboot it: the next boot mints a new ticket. Once a network boot has made it `install-unconfirmed`, abort is refused: `topf reset`, `machine reinstall --topf-reset-full-done`, then reboot.

**`render:approved` is critical.** An approved pair cannot be rendered, and `/config` answers 404 for it. The check names the cluster and host. If the alert `git_store_moved_aside` fired and the pair's tree is no longer upstream, approve a new pair.

**1Password errors.** A reference that failed waits for `cluster refresh`, not a retry timer. A whole-call failure (transport, token) retries from one minute to one hour, and a rate-limit error pauses retries until the next daily refresh. If `secrets:client` warns and the errors say "recovered by wazero", restart `serve`.

**`serve` stops naming the data key variable.** The key does not open the stored canary. Restore the key that sealed the database; there is no recovery without it.

[← Back to README](../README.md)
