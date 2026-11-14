# 08 - Talos cluster authoring: generation and secret handling

Researched 2026-11-12. Read-only. Citation keys:

- **booty** = `~/Projects/github/booty` @ `19eda9d` (main). Cited as `path:line`.
- **T** = `siderolabs/talos` @ `v1.14.6`, the latest stable (shallow clone in `/tmp/authoring-research/talos`). `M/` = `pkg/machinery/config/`.
- **O** = `siderolabs/omni` @ `v1.13.3` (2026-11-09; BSL 1.1, Change Date 2030-10-28, `LICENSE:1,10`).
- **TH** = `budimanjojo/talhelper` @ `v3.1.17`.
- **HO** = `~/Projects/gitlab/home-ops` `origin/main` @ `c8a370ccf` (2026-11-12). Structure only; no secret values were printed.
- **PROBE** = a throwaway Go program in `/tmp/authoring-research/probe` that ran machinery v1.14.6 (output quoted in §4).
- **INFERRED** marks my reading. **UNVERIFIED** marks claims I could not check against a source.

---

## 1. booty today (@19eda9d)

**Generation.** booty calls machinery directly, with no talhelper and no subprocess. Design §1.2 rejected talhelper because its node list duplicates booty's host registry (`docs/designs/2026-07-04-p6-talos-cluster-authoring-design.md:27-31`).
- `mintBundle` calls `secrets.NewBundle(secrets.NewFixedClock(2020-01-01), contract)` (`pkg/http/clustergen.go:122-136`).
- `generateNodeConfig` runs `generate.NewInput(...WithSecretsBundle, WithVersionContract(ParseContractFromVersion(talos_version)), WithInstallImage, WithInstallDisk("/dev/sda"))`, then `Config(type)`, then `configpatcher.Apply`, then `Validate(metal)` (`clustergen.go:199-249`).
- Patches are layered cluster → role → host (`clustergen.go:73-85`). They load through `LoadPatch`, never `LoadPatches`, so an `@file` patch cannot read the server's filesystem (`clustergen.go:87-101`).
- **Import** accepts only a `controlplane.yaml`, because a worker config "lacks the CA keys" (`pkg/http/clusterimport.go:42-60`). It rebuilds the bundle with `secrets.NewBundleFromConfig` (`pkg/http/api_clusters.go:760`).
- **Version contract** comes from the cluster's *current* `talos_version` (`clustergen.go:200`). A version bump therefore regenerates members under the new contract (design §6.5).

**Secrets at rest.**
- One age X25519 identity, read from the `--secretsKey` file, both encrypts and decrypts (`pkg/http/clustersecrets.go:12-51`, `pkg/config/secretskey.go:25-45`). It fails closed when unset and fails fast when malformed (design §7).
- `clusters.secrets_enc` holds the encrypted bundle (`pkg/db/migrations/0005_clusters.sql:16-26`).
- `cluster_node_configs.config_enc` holds frozen, age-encrypted per-member configs, and the table is never pruned (`0005_clusters.sql:28-42`). Each control-plane row carries every CA private key (issue #123). Every row is encrypted under the **same** key as the bundle (`api_clusters.go:450-458`).
- **Export** decrypts the bundle and returns `secrets.yaml` through `POST /clusters/{id}/export` (`api_clusters.go:243-262`).
- Design D3 freezes the bytes on purpose: same-contract regeneration is not guaranteed byte-identical across machinery builds (design §5, lines 142-159).

**Serving.**
- Route: `GET /machineconfig` (`pkg/http/http.go:54`). `serveFrozenNodeConfig` decrypts the frozen row on every request and writes it verbatim (`pkg/http/machineconfig.go:228-240`).
- The Talos iPXE line is `talos.config=http://[[server]]/machineconfig?mac=${mac}&<ticket>&uuid=${uuid}&serial=${serial}&hostname=${hostname}`. It is plain HTTP (`pkg/bootscript/pxe_config.go:9,18`).
- PR #167 (merged) added the ticket: `v1.<exp>.<sig>`, an HMAC-SHA256 over MAC, OS family and expiry, valid 1 h. Cluster members also keep an ARP check. Refusals are logged without the ticket.
- Residual risk noted in #167 (tracked as #166): anyone who asks `/booty.ipxe?mac=<approved MAC>` receives a valid ticket.

**Open issues.** #123 asks for a secret-store seam whose "done when" is "no CA private key is in SQLite". #124 is 1Password (`onepassword-sdk-go` v0.4.1, about 3.7 s start-up). #125 is OpenBao KV ("a PKI engine cannot keep them non-exportable"). #126 is bootstrap plus talosconfig/kubeconfig issuance. #127 adds control-plane nodes. #128 gates joins on a reachable control plane.

**Two findings booty does not record:**
1. **booty-minted CAs expire on 2029-12-29.**
   - `NewFixedClock(2020-01-01)` is passed into `NewTalosCA`/`NewKubernetesCA`/`NewEtcdCA`, which set `NotAfter = now + CAValidityTime (87600 h)` (T `M/generate/secrets/ca.go:22,34,47`; `secrets.go:15`).
   - PROBE confirmed it: os, k8s, etcd and aggregator all have `NotBefore=2020-01-01 NotAfter=2029-12-29`. Clusters created today get about 3.1 years of CA life instead of 10.
   - The fixed clock buys no reproducibility at mint time, because the keys are random (INFERRED).
2. **#126 talosconfig issuance would produce an already-expired cert.**
   - `unmarshalBundle` resets the clock to 2020-01-01 (`clustergen.go:163`).
   - `GenerateTalosAPIClientCertificate` uses `bundle.Clock.Now()` with a 365-day TTL (T `M/generate/secrets/bundle.go:460-471`; `constants.go:1397`). An admin cert would be valid only for 2020-01-01..2020-12-31.
   - INFERRED from code; confirm in the lab.

## 2. home-ops today (structure only)

- **Tool: talhelper.**
  - `talos/mod.just` runs `talhelper genconfig`, `talhelper gencommand apply -n <node> | bash` and `talhelper gencommand upgrade`. It calls `talosctl` directly for `upgrade-k8s`, `reboot`, `shutdown` and `reset`.
  - `talos/clusterconfig/` is git-ignored: `talosconfig` and `main-*` per-node configs. Rendered configs, including plaintext control-plane CA keys, exist only on the operator's workstation.
- **`talos/talconfig.yaml` (350 lines).**
  - Global: `talosVersion: v1.12.11` and `kubernetesVersion: v1.35.10`, both renovate-pinned. Also `clusterName`/`endpoint` with `${}` substitution from `talenv.yaml`, `cniConfig: none`, and shared cert SANs.
  - `nodes:` lists three control planes and two workers (one `arm64`). Each node has an install disk (by-id) or selector, network interfaces with bond/VIP, an inline `schematic:`, `nodeLabels` and `patches: ["@./patches/node/<host>/…"]`.
  - Top-level `patches:` takes 9 files from `patches/global/`. The `controlPlane:` and `worker:` groups each carry a schematic, labels and role patches (6 control-plane, 2 worker).
  - A node-level `schematic`/`nodeLabels` **replaces** the group's value rather than merging with it (file header comments, lines 14-19).
- **`talos/talenv.yaml`** is plaintext: domain, cluster name and VIP. It holds no secrets.
- **`talos/talsecret.sops.yaml`** is a whole-file SOPS-encrypted secrets bundle. Its keys mirror `secrets.Bundle`:
  - `cluster.{id,secret}`
  - `secrets.{bootstraptoken,secretboxencryptionsecret}`
  - `trustdinfo.token`
  - `certs.{etcd,k8s,k8saggregator}.{crt,key}`, `certs.k8sserviceaccount.key`, `certs.os.{crt,key}`
  - That makes 15 `ENC[...]` values, encrypted to one age recipient (sops 3.8.0). The `.sops.yaml` rule `.*\.sops\.ya?ml` has no `encrypted_regex`, so every value is encrypted. The `kubernetes/` rule encrypts only `data|stringData`.
- **How talhelper uses it.**
  - Defaults are `--secret-file talsecret{,.sops}.y{a,}ml` and `--env-file talenv{,.sops}.y{a,}ml` (TH `cmd/genconfig.go:73-74`).
  - It decrypts with the sops library (TH `pkg/decrypt/decrypt.go:11,62`), then calls the same machinery: `ParseContractFromVersion(talosVersion)` plus `generate.WithSecretsBundle` (TH `pkg/talos/input.go:23,32,86-87`).
  - The contract follows the *current* `talosVersion`, as in booty. talhelper v3.1.17 pins machinery `v1.14.0-alpha.2` (TH `go.mod:22`).
- **Other secret infrastructure in home-ops:**
  - 1Password via `onepassword-connect` and an external-secrets `ClusterSecretStore`.
  - The Flux SOPS age key is bootstrapped from `kubernetes/main/bootstrap/age-key.sops.yaml` (`docs/bootstrap.md:22`).
  - 1Password is already a running dependency.

## 3. Generation options

| | (1) Native machinery (booty) | (2) talhelper-style declarative | (3) Import-only from git | (4) Omni model |
|---|---|---|---|---|
| **Source of truth** | DB rows: cluster, spec patches, host binding, encrypted bundle, plus frozen output bytes | `talconfig.yaml` + `talsecret.sops.yaml` + patch files in git | git (home-ops); the service stores what it is handed | Omni state: `ClusterSecrets` bundle as JSON (O `controllers/omni/secrets/secrets.go:123,134,152`) and rendered `ClusterMachineConfig` resources (O `cluster_machine_config.go:401`) |
| **Per-node patches** | cluster → role → host strategic-merge/JSON6902 (`clustergen.go:73-85`) | global → role → node `@file` patches. Node `schematic`/`nodeLabels` replace the group's value | whatever git renders; the service does not patch | cluster, machine-set and machine config patches stored as resources, applied after `machineconfig.Generate` (O `cluster_machine_config.go:599-640`) |
| **Version contract on upgrade** | current `talos_version`; a bump re-renders under the new contract | current `talosVersion` (TH `input.go:23`) | git's choice | **initial** Talos version: `ParseContractFromVersion(input.InitialTalosVersion)` (O `client/pkg/machineconfig/machineconfig.go:21,47-52`). Upgrades never change the generated defaults |
| **Reproducibility** | byte-identical by freezing; regeneration is deterministic only per machinery build | deterministic per talhelper+machinery build; output is not committed | exact bytes, by definition | regenerates on input change and skips the update if the result is effectively equal (`configsEqual`, O `cluster_machine_config.go:391-404`) |
| **Git-ops fit** | weak: state lives in SQLite; `Export` is the only way out | strong, and it is what home-ops runs | strong, but the service is a dumb cache, and it needs a sync path plus a per-node render step somewhere that holds the keys | none: Omni is API-driven (it has its own templates and CLI) |
| **Delivery** | HTTP pull at `talos.config` | `talosctl apply` push (gencommand) | HTTP pull | push via `ApplyConfiguration` over SideroLink. The maintenance client uses `InsecureSkipVerify` and relies on the WireGuard tunnel (O `internal/backend/runtime/talos/clients.go:190-195`) |
| **Fit for the new service** | best: already ported, no subprocess, the library owns schema and validation | duplicates the host registry (design §1.2); useful as an *import format* | fits home-ops' `main` cluster, which already has a source of truth | right ideas (initial contract, render-on-change, redacted copies via `RedactedClusterMachineConfigs`), but BSL licensed: borrow ideas, not code |

Machinery itself changes generated *structure* by contract.
- At contract ≥1.14, generation moves Kubernetes, discovery identity and install settings into multi-doc documents: `MultidocKubernetesConfigSupported`, `DiscoveryIdentityMultidocConfig` and `UnattendedInstallConfig` all return `Greater(TalosVersion1_13)` (T `M/contract.go:272-285`).
- PROBE output: a worker config is 2 documents at 1.12 and 15 at 1.14; a control-plane config is 2 at 1.12 and 29 at 1.14.
- Under booty's current-version contract, a 1.13 → 1.14 bump would rewrite every member's config shape. Under Omni's initial-version contract it would not.

## 4. Secret inventory per config type

Sources: T `M/generate/secrets/secrets.go:21-59` (`Bundle`), `M/generate/init.go:39-41,134-170,212-238` (control plane; `controlPlane()` wraps `init()`), `M/generate/worker.go:33,38,111-113,128,166-170`, `M/generate/kubernetes.go:25-30,93-95,205-213`.

**PROBE run** (machinery v1.14.6; a real `NewBundle`, then for each config, which bundle values appear in the rendered output):

```
v1.12.11 worker:       [os.crt k8s.crt trustd.token bootstrap.token cluster.secret]
v1.12.11 controlplane: [os.key k8s.key etcd.key aggregator.key serviceaccount.key os.crt k8s.crt etcd.crt aggregator.crt trustd.token bootstrap.token cluster.secret secretbox]
v1.14.6  worker:       [os.crt k8s.crt trustd.token bootstrap.token cluster.secret]
v1.14.6  controlplane: (same 13 as 1.12)
worker rendered from a KEYLESS bundle == worker rendered from the full bundle: true (both contracts)
```

| Bundle field | Control-plane config | Worker config | What a thief can do with it |
|---|---|---|---|
| `Certs.OS` (Talos API CA) | crt **+ key** (`machine.ca`) | crt only (`worker.go:38`) | key: mint `os:admin` client certs, giving full Talos API control of every node |
| `Certs.K8s` (Kubernetes CA) | crt **+ key** (`cluster.ca` or `KubeAPIServerCAConfig.APIIssuingCA`) | crt only (`worker.go:128`, `kubernetes.go:210-213` `APIAcceptedCAs`) | key: mint `system:masters` kubeconfigs |
| `Certs.Etcd` | crt **+ key** (`cluster.etcd.ca`, `init.go:157`) | absent | read and write all cluster state |
| `Certs.K8sAggregator` | crt **+ key** (`kubernetes.go:25-30`) | absent | impersonate aggregated API servers |
| `Certs.K8sServiceAccount` | **key** (`kubernetes.go:93-95`) | absent | forge any service-account token |
| `Secrets.SecretboxEncryptionSecret` / `AESCBC…` | yes (`init.go:212-216`, `kubernetes.go:50-72`) | absent | decrypt Secrets stored in etcd |
| `Secrets.BootstrapToken` | yes | **yes** (`worker.go:113`) | join a rogue kubelet: Talos binds `system:bootstrappers:nodes` to node-client CSR auto-approval (T `internal/.../k8stemplates/kubelet.go:31`, `csr.go`) |
| `TrustdInfo.Token` | yes | **yes** (`worker.go:33`) | have trustd sign a **server-auth-only** cert. trustd strips Organization from the CSR, so it cannot mint a Talos client cert (T `internal/app/trustd/internal/reg/reg.go:70-92`) |
| `Cluster.ID` / `Cluster.Secret` | yes | **yes** (`worker.go:111-112,166-170`) | join or read discovery-service membership (and KubeSpan peers, if enabled) |

Conclusions:
- **A worker config holds no CA private key at any contract.** It is still a join credential: bootstrap token, trustd token and cluster secret. Treat it as secret, though at a lower tier.
- **A service can render worker configs from a bundle that holds only `{OS.crt, K8s.crt, TrustdInfo, BootstrapToken, Cluster}`.** `worker()` never dereferences the keys, and PROBE showed byte-identical output. booty's import rejects worker configs (`clusterimport.go:42-50`), but a worker config is enough to *rebuild a join-tier bundle*. `NewBundleFromConfig` nil-checks each field (T `M/generate/secrets/bundle.go:203-206`).
- **Control-plane configs, adding a control-plane node (#127), K8s CA rotation and minting talosconfig/kubeconfig (#126) all need the CA tier.**
  - Partial exception: with one long-lived admin talosconfig, the service can fetch kubeconfig through the Talos API (`talosctl kubeconfig`) without holding the K8s CA key. That cert still needs the OS CA key to mint, and it is itself an admin credential.

## 5. At-rest options

| Option | What protects the keys | Where the plaintext lives | Operational cost | Fit |
|---|---|---|---|---|
| **age key file** (booty today) | the file's mode and owner; one key for every row | service memory; the key file is on the same host as the DB | trivial; backups are safe only if the key is not backed up with them | fine for the join tier; weak for the CA tier (the DB plus the host disk equals every key) |
| **sops** (home-ops today) | the sops data key wrapped to age, PGP or KMS recipients; multi-recipient | wherever it is decrypted | a file format, not a store. Inside a service DB it adds nothing over age | the right **interchange format** for git import and export of `talsecret` |
| **OpenBao transit** (envelope) | the KEK never leaves OpenBao; the service asks it to unwrap a DEK | service memory, briefly | one more stateful service, plus an unseal story and an auth method (AppRole/JWT) | good: the DB holds ciphertext only, decryption is audited, and access can be revoked. #125 proposes KV instead; transit plus DB ciphertext is the alternative to weigh |
| **OpenBao KV** (#125) | OpenBao ACLs; the bundle lives in Bao | Bao, and service memory | same as above | good. #125 is right that the PKI engine cannot hold these: Talos needs the raw CA keys in configs |
| **1Password** (#124) | the 1Password vault; a service-account token on the host | service memory | the SDK start-up (about 3.7 s per #124) needs a reused client; already in the owner's stack | best for the **CA tier**: rarely read, human-recoverable, and the owner already runs it |
| **Offline CA tier** | an air gap; the operator supplies `secrets.yaml` per CA operation | operator workstation and request memory | a human in the loop for every control-plane operation | strongest; acceptable because control-plane operations are rare |
| **Render at request time** (instead of frozen configs) | removes N control-plane copies from the DB; only the bundle is stored | request memory | loses D3's byte-identity unless verified (see §7) | required to meet #123's "no CA private key in SQLite" |

Omni for comparison: all state is encrypted in etcd. The master key slot is unlocked by a PGP private key from `file://` or `vault://` (`privateKeySource: file:///omni.asc`). It stores the bundle **and** rendered control-plane configs (O `internal/backend/runtime/omni/state_etcd.go:95-142`, `loader.go:47-67`, `deploy/helm/omni/values.yaml:481-486`). Omni does not keep keys out of storage; it encrypts all of storage.

## 6. In-transit options (PXE → config)

| Mechanism | Confidentiality | Authenticates the server | Authenticates the node | Verified facts |
|---|---|---|---|---|
| **Plain HTTP + ticket** (booty) | none: the LAN sees the CP keys | no | ticket (HMAC, 1 h), plus ARP for members | `pxe_config.go:18`; PR #167 |
| **HTTPS `talos.config=`** | yes | yes, against `/etc/ssl/certs/ca-certificates.crt` | not by itself | The fetch uses `download.Download` → `httpdefaults.PatchTransport` with `RootCAs()` read from `DefaultTrustedCAFile = /etc/ssl/certs/ca-certificates.crt` (T `platform/metal/metal.go:100-110`, `pkg/download/download.go:227`, `pkg/httpdefaults/{httpdefaults.go:29-31,tls.go:26-56}`, `constants.go:1305-1308`). The pool is re-read whenever the file changes, and each attempt builds a new transport |
| **+ custom CA via `talos.config.early`** | yes | yes, with a private CA | no | Acquire order: STATE → embedded → `talos.config.early` → platform (`talos.config`) → `talos.config.inline` → maintenance (T `internal/app/machined/pkg/controllers/config/acquire.go:387-591`; docs `website/content/v1.14/reference/kernel.md:143-165`). A partial early config **is applied** (`SetConfig`, `acquire.go:217-226`) before the platform fetch. The docs cite `TrustedRootsConfig` as the example. `TrustedRootsController` writes the CA file from config (T `controllers/secrets/trusted_roots.go:59-80`). The cmdline limit is about 2000 bytes (docs), so use an ECDSA root. The early config is **replaced, not merged**, once the platform config arrives (no merge in `acquire.go`), so the served config must repeat the `TrustedRootsConfig`. That the CA file is written before the first fetch attempt is UNVERIFIED (download retries should cover the race) |
| **+ publicly trusted cert** (ACME DNS-01 on an internal name) | yes | yes, with the stock roots | no | needs no early config; INFERRED from the default trust file |
| **OAuth2 device flow** | over whatever transport `talos.config` uses | via TLS | yes: a token per boot, approved by a human or a policy | The parameters are **`talos.config.oauth.*`** (`client_id`, `client_secret`, `audience`, `scope`, `device_auth_url`, `token_url`, `extra_variable`) (T `constants.go:39-58`; `metal/oauth2/oauth2.go:48-110`). The v1.14 kernel reference says `talos.config.auth.*` (`kernel.md:139-141`), but the code and the docs.siderolabs.com OAuth page both say `oauth`, so the reference page is wrong. Talos sends `extra_variable`s (such as uuid and mac) to the device endpoint, prints the user code and a QR code on the console, polls, then sends `Authorization: <type> <token>` on the config GET (`oauth2.go:142-190`; `metal.go:82-109`). Defaults are `<talos.config>/device/code` and `/token` |
| **Maintenance + `apply-config --insecure --cert-fingerprint`** | yes (TLS) | the node is authenticated by pinning its self-signed cert's SPKI fingerprint | n/a: the operator pushes | The node logs the fingerprint and the exact command on its console (T `controllers/runtime/maintenance_service_inform.go:126-146`). The flag is in T `cmd/talosctl/pkg/talos/global/insecure.go:33`. Without the flag, `--insecure` accepts any cert. The fingerprint is only visible out of band (console), so it is manual and does not scale |
| **SideroLink** | WireGuard | yes | yes (join token) | `siderolink.api=` (T `constants.go:131`). Needs a SideroLink provision API server (Omni, or a reimplementation). Omni then pushes config over the tunnel (§3). Heavy |

**Ticket-in-URL leak paths:**
- Talos logs the populated URL on the console: `log.Printf("fetching machine config from: %q", downloadEndpoint)` (T `metal.go:69`). That reaches kmsg, the serial console and any configured log sink.
- The URL sits in `/proc/cmdline` for the life of the PXE boot. Most container runtimes do not mask `/proc/cmdline` from pods (UNVERIFIED for Talos and containerd).
- Over plain HTTP, the iPXE script and the GET are visible on the LAN, and proxy and access logs record query strings unless suppressed, as #167 does.
- The ticket is replayable for its 1 h lifetime by anyone who saw it. The ARP check is the only other binding, and only for cluster members.
- `${uuid}` alone is identification, not authentication. SMBIOS UUIDs are printed on labels and visible in BMCs, and cheap boards sometimes duplicate them (INFERRED, general knowledge).

## 7. Recommended design

### (a) Generation

1. **Keep native machinery (option 1) as the engine.** Port `clustergen.go`/`clusterimport.go` close to verbatim; the library owns schema and validation.
2. **Pin the version contract to the cluster's *initial* Talos version**, as Omni does. Store `contract_version` separately from `talos_version`. A bump changes the install image and the netboot pin, never the generated shape.
3. **Drop the fixed 2020 clock.** Mint with `time.Now()` and generate client certs with `time.Now()`. It buys nothing and it costs CA lifetime (§1, findings 1-2). Ported clusters keep their 2029-12-29 expiry, which needs a planned rotation (point (b)5).
4. **Render on request; do not freeze control-plane configs.**
   - Keep D3's guarantee by storing only the **sha256 of each member's last approved render**. A hash is not secret.
   - On serve, render and compare. On mismatch (a machinery upgrade or changed inputs), refuse and show "config drift: approve new revision" in the admin UI.
   - Worker renders can be frozen or rendered as preferred, because they hold no CA keys.
5. **Import formats:**
   - (i) `controlplane.yaml`, as today.
   - (ii) `talsecret.sops.yaml` or `secrets.yaml` plus a cluster spec. talhelper's `talsecret` keys are `secrets.Bundle` field names, so it is a direct load.
   - (iii) A *worker* config, to build a join-tier-only cluster record that can add workers to a cluster whose CA stays elsewhere.
   - Do not adopt talhelper's node list (design §1.2 still holds).
6. **home-ops `main` cluster:** the owner decides (below) whether it stays talhelper-authored, with the service at most serving worker joins from an imported join-tier bundle, or moves to service authoring.

### (b) Securing

1. **Split the bundle into two tiers behind the #123 seam.**
   - **Join tier** `{OS.crt, K8s.crt, TrustdInfo.Token, BootstrapToken, Cluster.ID/Secret}`, online, age-encrypted in SQLite (the first backend, as today).
   - **CA tier**, the full bundle, held in 1Password (#124) or OpenBao (#125), or offline. It is fetched only for an admin-initiated control-plane operation (create, add control plane, rotate, issue talosconfig) after OIDC/passkey step-up. It stays in memory only for that operation and is never written to SQLite.
   - Result: #123's "no CA private key in SQLite" holds, and a stolen service host yields worker-join power, not cluster-admin.
2. **Serve control-plane configs only inside an armed window.**
   - The admin arms a host in the UI. The service fetches the CA tier, renders, and serves exactly one fetch for that host (or pushes it).
   - The window then closes and the key material is dropped. Control-plane PXE boots are rare, so the human step is cheap.
3. **Transport, layered:**
   - **L1** HTTPS on `talos.config=`. Prefer a publicly trusted cert (ACME DNS-01), because Talos's stock roots then work with no cmdline payload. Otherwise use a private ECDSA root via `talos.config.early` `TrustedRootsConfig`, and repeat it in the served config. The service's OCI registry needs TLS anyway, so share the certificate machinery.
   - **L2** Node proof: keep the boot ticket, but bind it to the SMBIOS UUID, cut the TTL to the boot window (minutes), and make it **single-use** (consumed on the first 200). This blunts the console-log and `/proc/cmdline` leaks.
   - **L3** Approval gate: there is no config for unapproved UUIDs.
   - **L4** For control-plane nodes, optionally the OAuth2 device flow (`talos.config.oauth.*`) with the service as the device-auth server. Talos sends uuid and mac as `extra_variable`s, the admin approves with a passkey, and the token is valid for one fetch. It is built into Talos, so it needs no agent.
   - Skip SideroLink: it requires reimplementing Omni's provision API. Keep maintenance-mode `--cert-fingerprint` as the manual fallback for when PXE config fails.
4. **Logging:** never log the ticket or the query string (port #167's rule). Treat the Talos console as a leak surface, which is why single-use matters.
5. **Rotation.**
   - `talosctl rotate-ca` mints a fresh bundle in memory (`secrets.NewBundle(...time.Now(), TalosVersionCurrent)`, T `cmd/talosctl/cmd/talos/rotate-ca.go:89`), patches the nodes, and writes **only a talosconfig** (`rotate-ca.go:159-161`).
   - It rotates only the Talos API CA and the Kubernetes API issuing CA. The aggregator CA, etcd and the SA key are manual (`rotate-ca.go:40-44`; docs.siderolabs.com CA rotation: "update `secrets.yaml` … with new CA key and certificate").
   - A stored bundle therefore goes stale silently.
   - The service should run the rotation itself through the library packages `pkg/rotate/pki/{talos,kubernetes}`. Steps: add as accepted → verify → make issuing → verify → remove old (T `pkg/rotate/pki/talos/talos.go:75-85`).
   - It should persist the new CA tier as a **pending revision before step 1** and promote it after the final verify, so a crash can never leave the only copy of a live CA in memory. The join tier changes too, because the CA crts do, so re-render or re-freeze workers.
   - Omni patches `MachineAcceptedCAs` and `K8sAddAcceptedCA` during rotation (O `cluster_machine_config.go:620-636`). A recovery path is `talosctl gen secrets --from-controlplane-config` (T `cmd/talosctl/cmd/mgmt/gen/secrets.go:101`), which is `NewBundleFromConfig` on a live control-plane config.

### What the owner must decide

1. CA-tier home: 1Password (already deployed), OpenBao (new service), or offline with a human supplying it per operation.
2. Whether control-plane configs are ever served unattended. The recommendation is no: armed window, optionally with the device flow.
3. Whether home-ops `main` stays talhelper-authored (the service serves at most worker joins) or migrates to service authoring.
4. Whether render-plus-hash-verify is an acceptable substitute for D3's frozen bytes.
5. TLS source: public ACME DNS-01 or a private root delivered with `talos.config.early`.

### What to lab-test

1. Talos 1.14 PXE with `talos.config.early` carrying `TrustedRootsConfig` (size after `zstd -22 | base64`; that it fits iPXE and the about 2 KB limit), then an HTTPS `talos.config` against a private CA. Expect success on the first or a retried attempt.
2. Single-use ticket: a second GET returns 404, and Talos's retry loop does not burn the ticket before the network is up.
3. OAuth device flow against a minimal device-auth endpoint. Confirm the `extra_variable` names and values that arrive, and the token header on the config GET.
4. Render-on-request hash stability: render the same inputs on machinery 1.14.x and 1.15.x under a pinned 1.12 contract and compare hashes. This measures how often drift approval would fire.
5. A worker join from a keyless bundle against a real cluster, and `NewBundleFromConfig(worker.yaml)` producing a usable join tier.
6. Whether pods on Talos can read `/proc/cmdline`, which settles the ticket-leak severity.
7. A booty-ported cluster: confirm the CA `NotAfter` of 2029-12-29 on a real node (`talosctl get mc` or `openssl x509`), and plan a `rotate-ca` dry run.
