# Signer key provenance

The four public keys in this directory authenticate the catalog entries whose
upstream publishes a detached signature. `task catalog:pin` verifies each
object (or its checksum list) with gpg against the key file named by the spec's
`Signature.Key`, then requires the signer's **primary** fingerprint to be the
one `gpg --show-keys` reads from that file **and** to appear in this document.
A fingerprint that is not written here fails the entry with `signature:`.

Every key was fetched on 2026-11-16 during the M2 pin spike, imported into a
fresh gpg keyring in a container, and used to verify the real upstream
signature (`VALIDSIG`). The independent sources are hosts other than the one
that serves the signed object; a source that also hosts the checksums is listed
but does not count.

| Entry | Key file (sha256) | Primary fingerprint | Signing subkey | UID | Expiry | File source | Independent sources |
|---|---|---|---|---|---|---|---|
| systemrescue | `systemrescue.asc` (`5f64ae72bdd73009aee57abe87440d2490047aebf8bff990f1007712299dd93d`) | 0FF11AF081E98345594812037091115F8320B897 | 62989046EB5C7E985ECDF5DD3B0FEA9BE13CA3C9 | Francois Dupoux 20210704 (Generated on 20210704), no e-mail | 2039-12-31 | GitLab `systemrescue/systemrescue-website`, `static/security/signing-keys/gnupg-pubkey-fdupoux-20210704-v001.pem`, commit `46a703d2` (fdupoux, 2021-12-27); byte-identical to the www.system-rescue.org copy | (1) the GitLab repository above, a different host from the ISO mirrors; (2) keyserver.ubuntu.com, same key (an unauthenticated keyserver). keys.openpgp.org holds this key without a UID, so gpg cannot import it from there and it is never used |
| clonezilla | `clonezilla.asc` (`908b504cbd48d02d8023548d2a8b92950262d8f9771ef6afc652ff29fd13f98e`) | 54C0821A48715DAFD61BFCAF667857D045599AFD | none: the primary signs | DRBL Project (Diskless Remote Boot in Linux) <drbl@nchc.org.tw>, <drbl@clonezilla.org> | none | keys.openpgp.org, `/vks/v1/by-fingerprint/54C0821A48715DAFD61BFCAF667857D045599AFD` | (1) keys.openpgp.org, which verified both e-mail UIDs; (2) drbl.org `GPG-KEY-DRBL`; (3) keyserver.ubuntu.com. clonezilla.org/gpg-verify.php prints the fingerprint but also hosts CHECKSUMS.TXT, so it does not count |
| gparted | `gparted.asc` (`a98c4363345fe7d22c10948a0c3d4b49ee08df7028fba456f70599855a94154c`) | EB1DD5BF6F88820BBCF5356C8E94C9CD163E3FB0 | none: the primary signs | Steven Shiau (In Freedom We Trust) <steven@stevenshiau.org>, and six other UIDs including <steven@clonezilla.org> | none | keys.openpgp.org, `/vks/v1/by-fingerprint/EB1DD5BF6F88820BBCF5356C8E94C9CD163E3FB0` | (1) keys.openpgp.org, which verified the UIDs; (2) keyserver.ubuntu.com. gparted.org/gpg-verify.php prints the fingerprint but also hosts CHECKSUMS.TXT, so it does not count |
| grml | `grml.asc` (`9a89bf98772d8ff25d74749051e19f74508c714219cf2f07434bf882ab7977ae`) | 33CCB136401AFEC843A3876396A87872B7EA3737 | none: the primary signs | Michael Prokop <mika@grml.org>, <mika@debian.org>, and three other UIDs | **2028-04-21** | keys.openpgp.org, `/vks/v1/by-fingerprint/33CCB136401AFEC843A3876396A87872B7EA3737` | (1) Debian `debian-keyring` 2025.07.26, `/usr/share/keyrings/debian-keyring.gpg`, apt-signed; (2) keys.openpgp.org, which verified the UIDs; (3) grml.org/download, a different host from ftp-master.grml.org and the mirrors, which prints the same fingerprint |

## What each key signs

| Entry | Signed object | Spec field |
|---|---|---|
| systemrescue | the ISO itself (`systemrescue-<version>-amd64.iso.asc`) | `Signature.SigURL` |
| clonezilla | `CHECKSUMS.TXT` (`.gpg`), which lists the zip's sha256 | `Signature.SigURL`, `ChecksumsURL` |
| gparted | `CHECKSUMS.TXT` (`.gpg`), which lists the zip's sha256 | `Signature.SigURL`, `ChecksumsURL` |
| grml | `SHA256SUMS-<version>` (`.gpg`), which lists both ISOs | `Signature.SigURL`, `ChecksumsURL` |

memtest86plus, shredos and uefishell are trust on first use: upstream signs
nothing the pin could check (memtest86plus and shredos publish an unsigned
checksum list from the same host as the object, which the pin still requires
the object to match).

## Expiry

The grml key expires on **2028-04-21**. A re-pin after that date needs a
refreshed key: fetch the new copy from keys.openpgp.org by fingerprint, check
it against the independent sources above, replace `grml.asc` and update this
table. Until then an `EXPKEYSIG` result makes the entry absent with
`signature:`; the tool never accepts a signature from an expired key.

## Refreshing a key

SystemRescue signs with a subkey, so the check compares the primary
fingerprint (the last field of gpg's `VALIDSIG` line), never the subkey. Check
a replacement file with `gpg --batch --with-colons --show-keys <file>`: the
first `fpr` record is the primary and must equal the table above.
