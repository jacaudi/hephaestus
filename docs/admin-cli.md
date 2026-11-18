# Admin CLI

This page is the reference for the `hephaestus` admin commands: `boot-next`, `utility` and `version`.

The same binary is the admin CLI. Run it inside the container, where it reads the same `HEPHAESTUS_` environment as `serve` and writes the same database; it never opens the content store, so `serve` stays the only process that fetches and serves content.

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

Shell access to the container is the authorization; the CLI has no network surface and no credentials of its own. It refuses to run, with exit status 1, while the database has pending migrations: start `hephaestus serve` once to apply them. A wrong number of arguments or an unknown flag exits with status 2.

The `--log-format` flag is available on every command; it applies to `serve`.

[← Back to README](../README.md)
