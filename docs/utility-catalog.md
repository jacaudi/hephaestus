# Utility catalog

This page is the reference for the utility catalog and the task guide for re-pinning it with `task catalog:pin`.

The utility operating systems a machine can boot into (memtest86+, SystemRescue, Clonezilla, GParted, ShredOS, grml, the UEFI shell) are described in `internal/catalog/catalog.go`. Their sizes, hashes, extracted paths and pin ids are generated into `internal/catalog/catalog_pins.go` by:

```sh
task catalog:pin                      # pin every entry
task catalog:pin -- -only grml        # re-pin one entry; every other entry keeps its pin or absent reason
```

`task catalog:pin` runs inside a pinned container (`task catalog:image` builds it: the `catalogpin` tool plus gpg), so the host needs no gpg. It needs `DOCKER_HOST` set for colima, the internet and about 6.5 GB of downloads. For each object it downloads it over TLS 1.3 (every redirect hop is checked), checks the size, extracts the files the catalog boots, and records their hashes. The EFI-stub check reads the PE header where the DOS header points (`e_lfanew`, bounded to 64 KiB), because real EFI binaries, such as the UEFI Shell's AArch64 build, put it well past byte 512. A checksum list with `### <ALGO>SUMS:` section headers is read only in the wanted algorithm's section: the Clonezilla and GParted lists carry a 64-hex `B3SUMS` BLAKE3 section that is as long as a sha256. A signed entry is verified against the key in `internal/catalog/keys/` before anything is parsed; a trust-on-first-use entry is pinned on first use. The report, with every hop, hash, signer, the ShredOS EFI-stub finding and whether each recipe's operating system honours BOOTIF, is `build/catalogpin/report.txt`.

An entry that cannot be pinned is not an error. It goes in the `absent` ledger of `catalog_pins.go` with a reason that starts with its class:

| Class | Meaning |
|---|---|
| `tls13:` | a hop did not complete TLS 1.3 |
| `network:` | DNS, connect, reset, timeout, another transfer failure, a local gpg failure such as no gpg, or a broken/missing committed key (up to 3 attempts for a transfer) |
| `http-<status>:` | an unexpected HTTP status (`5xx` and `429` get up to 3 attempts) |
| `integrity:` | a size, hash or extraction failure |
| `signature:` | signature verification failed, the signer is not the committed key's primary, or the signer is not in `PROVENANCE.md` |
| `checksum:` | the upstream checksum list does not name the object's digest |

`internal/catalog/keys/PROVENANCE.md` records where each signer key came from and who corroborates it. **The grml key expires on 2028-04-21**: a re-pin after that date needs a refreshed `grml.asc` first.

[← Back to README](../README.md)
