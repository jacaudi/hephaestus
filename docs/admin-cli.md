# Admin CLI

This page is the reference for the `hephaestus` admin commands: `cluster`, `machine`, `audit`, `unknown-action`, `boot-next`, `utility` and `version`.

The same binary is the admin CLI. Run it inside the container, where it reads the same `HEPHAESTUS_` environment as `serve` and writes the same database; it never opens the content store or the git object store and never calls the network, so `serve` stays the only process that fetches, resolves, renders and serves. A command that needs one of those writes a request to the database, and `serve` picks it up within 10 seconds.

```sh
docker exec hephaestus /hephaestus boot-next set 52:54:00:12:34:56 systemrescue
```

| Command | Effect |
|---|---|
| `boot-next set <mac\|uuid> <entry>` | Boots the machine into the utility on its next PXE boot, once. The machine must have booted through hephaestus before. A UUID that several machines share is refused with the list of their MACs; name one by MAC. A machine identified only by its MAC (a junk or shared UUID) is refused when named by UUID: name it by MAC, which binds its flag to that MAC so that only a boot from it is served the utility (`boot-next set` and `boot-next list` show the bound MAC). An entry with no build for the machine's architecture is refused. That check judges architecture only (so does `boot-next list`): the firmware platform is known only at boot, so `serve` may still clear the flag as `unsupported` for a platform the entry lacks. It asks `serve` to fetch the utility, prints when the flag expires and the utility's state (`wanted`, `fetching`, `ready`, `failed`, `broken` or `stale_pin`), re-arms a failed or broken one and shows its previous error, and warns first for an entry that erases disks (ShredOS) |
| `boot-next clear <mac\|uuid>` | Cancels the machine's boot-next. It exits 0 with a note when there is none, or when the flag changed while the command ran |
| `boot-next list` | Every flag: machine, MACs, entry, architecture, when it was set, its age in seconds and its status: `expired`, `unknown_entry`, `unsupported`, or the utility's state (`not_requested` when nothing has asked for it) |
| `utility fetch <entry> [--arch amd64\|arm64]` | Asks `serve` to fetch the entry ahead of any boot, for every architecture it is built for or the one named; re-arms a failed or broken fetch |
| `utility list` | Every catalog entry by architecture: version, platforms, fetch state (`stale_pin` when the stored row is for an older pin), progress (of the object being fetched now), last error (cleaned text, cut to 1 KiB), last change (it moves only when the state does, not as progress advances), whether its content is trust-on-first-use, and the minimum RAM in MiB |
| `version` | Prints the version, commit and build date of the binary (`--json` for a JSON object) |

Every `boot-next` and `utility` command takes `--json`. A boot-next flag expires 24 hours after it was set: a machine that boots later boots as usual, and the flag is cleared when it does.

## Clusters

A cluster is a Topf definition in an https git repository. `serve` fetches and renders it; the commands below store the definition and approve what `serve` rendered. The walkthrough is in [Install Talos](first-install.md).

| Command | Effect |
|---|---|
| `cluster import <name> --repo <url> --ref <branch> --path <dir> [--username <name> --credential ref+op://…]` | Stores the cluster. The name is a DNS label. The URL must be https and carry no user or password; `--ref` is a branch to follow, not a tag. A private repository takes a username and one `ref+op://` reference to its password or token |
| `cluster set <name> [--repo … --ref … --path … --username … --credential …]` | Changes the fields given. A changed repo, ref or path discards the pending pair and fetches the new source at once; the approved pair keeps serving until a new one is approved. An empty value clears a username or credential |
| `cluster remove <name>` | Deletes the cluster with its pairs and stored renders. Refused while any machine is bound to it |
| `cluster list [--errors]` | Every cluster: source, revision, approved and pending pair ids, failed tree, last fetch, whether the last refresh was incomplete, and whether an error text is stored. `--errors` prints the texts, which can quote a fragment of a secret value: keep that output out of tickets and logs |
| `cluster diff <name>` | Per host, the redacted diff of the pending pair against the approved one, and the pair id `cluster approve` takes. A host whose redacted render is unchanged but whose real config changed shows "secret values changed". It lists the 1Password references the pending pair resolved and the approved pair did not. Secret values are never shown |
| `cluster diff <name> --host <host>` | The drift of that host of the approved pair: its stored render against the running one, and the hash to approve |
| `cluster approve <name> <pair id>` | Approves exactly the pair `cluster diff` showed. It deletes the older pair, revokes every live ticket of the cluster, orphans the machines whose host the new pair lacks, and wants the Talos files of every bound machine whose schematic or version changed. A newer render makes the id stale, and the command refuses it |
| `cluster approve <name> --host <host> --hash <hash>` | Approves a drift: the running config becomes the stored one and the host is released. The pair stays approved and no ticket is revoked |
| `cluster refresh <name>` | Asks `serve` to fetch the cluster and resolve its references again, and clears the retry backoff. It is audited |

## Machines

A machine is named by its MAC or its UUID, and must have booted through hephaestus once (`adopt --uuid` creates one). A host is named `<cluster>/<host>`. Every change is audited with the actor `cli`.

| Command | Effect |
|---|---|
| `machine list` | Every machine with its state |
| `machine show <mac\|uuid>` | State, binding and Talos tuple, the state of the Talos files it installs from, and its live config ticket: when it was minted and whether `/config` was ever asked for with it. The token is never shown |
| `machine approve <mac\|uuid> <cluster>/<host>` | Binds a discovered machine to an unbound host of the approved pair and assigns it: its next boot installs Talos. Its Talos files are wanted at once. Refused for a junk UUID |
| `machine adopt <mac\|uuid> <cluster>/<host>` | Binds a discovered machine that already runs Talos as `installed`; nothing is installed |
| `machine adopt --uuid <uuid> --arch <amd64\|arm64> <cluster>/<host>` | The same for a machine that has never booted through hephaestus. It creates the record, with its MACs, serial and product filled in by its first boot |
| `machine fetch <mac\|uuid> [--reset-hash]` | Wants the Talos files of the machine's host, and re-arms a `failed` or `broken` row. `--reset-hash` clears the recorded sizes and hashes so the next fetch records afresh from any state: a changed upstream build is a trust decision, and this accepts it. Audited |
| `machine mark-installed <mac\|uuid>` | Confirms an `installing` or `install-unconfirmed` machine as `installed`; its next boot exits to its disk |
| `machine abort <mac\|uuid>` | Returns an `installing` machine to `assigned`, so its next boot mints a new ticket. Refused once it is `install-unconfirmed` |
| `machine reinstall <mac\|uuid> --topf-reset-full-done` | Returns an `installed` or `install-unconfirmed` machine to `assigned`. Refused without the flag; see [Reinstall](first-install.md#reinstall) for the order |
| `machine unassign <mac\|uuid>` | Returns an `assigned` machine to `discovered` |
| `machine unbind <mac\|uuid>` | Returns an orphaned machine, whose host left the definition, to `discovered` |
| `machine rebind <mac\|uuid> <cluster>/<host>` | Binds an orphaned machine to an unbound host, its own former one included. It lands `installed` if it was installed before, else `assigned`. Never for a junk UUID |
| `machine cordon <mac\|uuid>`, `machine uncordon <mac\|uuid>` | Sets a machine aside, remembering its state, and lifts that |
| `machine local-only <mac\|uuid>`, `machine unmark-local-only <mac\|uuid>` | Makes a machine always boot its local disk, and undoes it |
| `machine reject <mac\|uuid>`, `machine unreject <mac\|uuid>` | Turns a discovered machine away, and undoes it |
| `machine forget <mac\|uuid>` | Deletes a rejected, discovered or local-only machine; it is recorded again if it boots |

## Audit and Settings

| Command | Effect |
|---|---|
| `audit list [--since <duration>] [--machine <mac\|uuid>]` | The audit events, oldest first: when, who, what, the machine and the detail. `--since` takes a Go duration such as `90m` or `72h`. Alerts are audit events, so this is where they show; see [Observability](observability.md#alerts) |
| `unknown-action set <hold\|local>` | What a machine with no record, or `discovered`, does at boot: `local` boots its disk, `hold` waits for an approval. `hold` first lists the unbound hosts of the approved pairs and every `discovered` machine seen in the last 7 days, changes nothing, and runs only when repeated with `--yes` |

`cluster`, `machine`, `audit` and `unknown-action` commands take `--json`.

Shell access to the container is the authorization; the CLI has no network surface and no credentials of its own. It refuses to run, with exit status 1, while the database has pending migrations: start `hephaestus serve` once to apply them. A wrong number of arguments or an unknown flag exits with status 2.

The `--log-format` flag is available on every command; it applies to `serve`.

[← Back to README](../README.md)
