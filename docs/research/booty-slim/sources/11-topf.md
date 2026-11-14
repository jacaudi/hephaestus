# 11 - Topf (postfinance/topf) as the talhelper replacement

Subject: `github.com/postfinance/topf`, MIT. Clone in `/tmp/topf-research/topf`.
Pinned: tag `v0.6.2` = `70cdbc8` (2026-10-23). `main` = `ee2abaa` (2026-11-11). Since the tag,
`main` changed only `CHANGELOG.md`, `cmd/topf/root.go` (shell completion shown in help) and
`docs/configuration.md` (schematic doc fix, `e5adba0`). Every code claim below holds on `main` too.

Method: read the source and docs. Built `topf` from `v0.6.2` and ran `render` against fake labs
(`/tmp/topf-research/lab*`). Wrote one external-module Go probe (`/tmp/topf-research/probe`).
Every secrets bundle was freshly generated and fake. No Talos node was contacted.
home-ops: read with `git show origin/main:` / `git ls-tree`. I ran one `git fetch origin main`, which
updated local remote-tracking refs only. Nothing was decrypted and no values were printed.

Citations: `path:line` is at `@v0.6.2` unless marked. `sib:04` = `04-talos-upstream.md` in this
directory. **INFERRED** = concluded, not read. **UNVERIFIED** = could not confirm.

## 0. Headline findings

1. **Rendering is `internal/`.** `generateNodeConfig` lives in `internal/topf/nodes.go:136-215`.
   `pkg/config` is public, but its `PatchContext.Load` takes a `*decryption.Cache` from
   `internal/decryption` (`pkg/config/patches.go:34`). An external module cannot build one. With a nil
   cache, any non-`.tpl` patch panics (probe: "nil pointer dereference").
2. **Output is deterministic.** Same repo, same secrets and same contract give byte-identical files
   across runs. My external re-implementation also matched the `topf render` bytes
   (sha256 `c363887…` cp, `28f37db…` worker). The clock is not an input. This suits D14's hash.
3. **The version contract is the *current* Talos version.** It is not pinned to the cluster's
   initial version. The fallback chain is running node version, then `topf.yaml talosVersion`, then
   the bundled machinery tag (`internal/topf/node.go:47-49`). A 1.14 contract produced 286 lines over
   about 30 documents. A 1.13 contract produced a 98-line single `v1alpha1` document. So the contract
   dominates the hash.
4. **The Go module proxy serves a different `v0.6.2`.** proxy.golang.org reports
   `v0.6.2 = 7aeead7` (2026-10-14, machinery v1.14.1). The git tag and the GitHub release are at
   `70cdbc8` (machinery v1.14.4). `go get github.com/postfinance/topf@v0.6.2` therefore builds
   different Talos code than the release binary. INFERRED: the tag was moved after the proxy cached it.
5. **Pushing needs the CA keys.** `topf apply/upgrade/reset` mint an admin client from the full
   secrets bundle (`internal/topf/client.go:31-36,43-77`). Any Topf-driven day-2 operation needs the
   CA private keys on the machine running Topf.

## 1. Identity and maturity

- **Releases.** 16 final tags: v0.1.0 (2026-02-10) to v0.6.2 (2026-10-23), plus an `-rc` before most
  of them (`git tag`; `gh release list`). That is roughly one minor release every 4-6 weeks, with patch
  releases in between. First commit 2025-12-24. 208 commits on `main`.
- **Contributors and bus factor.** Non-merge, non-bot commits: Clément Nussbaumer 116, Sebastian
  Stephan 27, everyone else 4 or fewer (`git log --no-merges`). Since 2026-08-12: 25 and 13
  respectively. Both are PostFinance addresses. **Bus factor is 2, effectively 1.** The talhelper
  author (budimanjojo) has one commit.
- **Production use.** Per `docs/production-usage.md:49-56`, PostFinance runs Topf in GitLab CI. A nodes
  provider calls an internal inventory system. A secrets provider calls the corporate secrets tool.
  A shared config repo is pulled through `data.configSource`. The docs claim this; I cannot verify
  scale (UNVERIFIED). The "early stage project" warning was removed in 0.6.0 (CHANGELOG on `main`).
- **Go and Talos.** `go 1.27.0` (`go.mod:3`). Built on `siderolabs/talos` and `pkg/machinery`
  **v1.14.4** (`go.mod:14-15`). It renders any contract that `ParseContractFromVersion` accepts. I
  rendered 1.12.11, 1.13.4 and 1.14.4. `upgrade` falls back to the legacy RPC below 1.13
  (`internal/cmd/upgrade/upgrade.go:312-332`). Doc drift: `docs/getting-started.md:3-9` still says
  "Talos v1.13.x" and "built against v1.13 machinery".
- **Tests.** `test.yml` runs `go test -v -race ./...` with sops, age and vals installed. There are
  about 20 top-level unit tests across `pkg/config`, sops, vals, decryption, maskedwriter, schematic
  and nodepool. `e2e.yml` boots a QEMU cluster with 3 control planes on Talos v1.14.0
  (`test/e2e/cluster.yaml:15`). Build tag `integration` covers bootstrap, upgrade, dry-run-clean-after-apply
  and a label patch lifecycle (`internal/integration/*_test.go`). **No test pins rendered output
  (golden files).** Nothing would catch a byte change in a release.

## 2. Config model

**`topf.yaml` schema** (`pkg/config/topf_config.go:19-57`): `clusterName`, `clusterEndpoint` (a URL),
`kubernetesVersion`, `talosVersion?`, `schematicId?`, `factory?` (default `factory.talos.dev`),
`platform?` (default `metal`), `secureboot?`, `secretsProvider?`, `nodesProvider?`, `patchesDir?`
(default: the config file's directory), `secretsPath?` (default `<dir>/secrets.yaml`), `nodes[]` and
`data` (free-form map). `configDir` is a hard error (`:79-81`). Unknown keys are silently ignored
(non-strict `yaml.Unmarshal`, `:74`; INFERRED).
**Node** (`pkg/config/node.go:14-34`): `host` and `role` are required (`:64-70`); `role` is
`control-plane` or `worker` (`node_role.go:11-17`). Optional: `ip`, `data`, `talosVersion`,
`schematicId`, `factory`, `platform`, `secureboot`. `runtimeData` (talosVersion, schematicId, stage)
is filled from the live node. Nodes are sorted control planes first, then by host (`topf_config.go:123-131`).
`topf.yaml` itself goes through SOPS and vals (`:67`) but **not through Go templates**.

**Patch layout and precedence** (`pkg/config/patches.go:40-65`): `all/`, then `<role>/`, then
`node/<host>/`. `<host>` must match `nodes[].host` exactly. Files are walked lexically and recursively,
symlinks are followed (`:67-118`), and they must match `.*\.ya?ml(\.tpl)?` (`:70`). A missing folder is
fine (`:113-114`). Each file can hold several YAML documents. Empty documents are skipped. A JSON-patch
array is rejected (`:190-238`, `:212-214`). Later patches win (`docs/configuration-model.md:16`).
Topf prepends its own installer patch **before** all user patches (`internal/topf/nodes.go:176`), so
any user patch overrides it.

**Templating**: only `*.tpl` files are templated. They use `text/template`, sprig and
`missingkey=error` (`patches.go:147-166`). Templates **skip SOPS and vals** (`:175-185`;
`configuration-model.md:60`). The context is the `PatchContext` (`patches.go:25-35`): `.ClusterName`,
`.ClusterEndpoint`, `.KubernetesVersion`, `.TalosVersion` (always the cluster value, `nodes.go:145`),
`.SchematicID` (resolved), `.Data`, `.Node.{Host,IP,Role,Data,RuntimeData.*}` and `.PatchesDir`.
`RuntimeData` is empty when rendering offline (`configuration-model.md:80-84`). sprig `env` makes
output depend on the process environment.

**Schematic and installer**: `schematicId` is either a hash or `@file[.tpl]`. The ID is computed
locally, or submitted with `--submit-to-factory` (`docs/configuration.md:89-151`,
`internal/schematic/resolver.go:110-140`). Installer image:
`<factory>/<platform>-installer[-secureboot]/<schematic>:v<talosVersion>`. Each field resolves node,
then cluster, then default (`internal/topf/node.go:78-91`).

**Talos 1.14 multi-doc**: if the contract supports `UnattendedInstallConfig`, Topf injects an
`UnattendedInstallConfig` with the installer image, `wipe: false` and a **default disk selector
`disk.dev_path == "/dev/sda"`** (`nodes.go:29,36-48`). Older contracts get `machine.install.image`
instead (`:49-56`). `UnattendedInstallConfig` and `.machine.install` exclude each other
(`configuration-model.md:139`). `upgrade` reads the image from `UnattendedInstallConfig` first
(`node.go:136-148`, the 0.6.2 fix).
Quirk (INFERRED bug): a node-level `talosVersion` changes the installer tag (`node.go:83`) but **not**
the contract (`node.go:47-49`) or `.TalosVersion`.

## 3. How it generates configs

- It calls Talos machinery directly: `bundle.NewBundle(WithInputOptions{ClusterName, Endpoint,
  KubeVersion, GenOptions: generate.WithSecretsBundle, generate.WithVersionContract},
  WithPatchControlPlane|WithPatchWorker(patches))` (`internal/topf/nodes.go:185-207`). It never calls
  `generate.NewInput` itself; the bundle does that internally (`pkg/machinery@v1.14.4/config/bundle/bundle.go:115`). Output
  is `EncodeBytes(CommentsDisabled, OmitEmpty)` (`cmd/topf/render.go:72-75`). `apply` sends the same
  bytes (`internal/topf/apply.go:38`).
- **Version contract**: `ParseContractFromVersion(node.TalosVersion())` (`nodes.go:166`). The chain is
  the running version (when online, which covers `apply`, `upgrade`, `reset` and `render --online`),
  then `topf.yaml`, then bundled `version.Tag` (`node.go:47-49`; `docs/commands/render.md`). It is
  **not** pinned to the cluster's original version. Raising `talosVersion` changes every node's
  offline render.
- **Determinism (lab-verified)**: `topf render` run twice gives identical sha256 (`lab/out1`, `out2`).
  The external probe gave identical bytes even though it was built on machinery v1.14.1 against a
  v1.14.4 binary, and it used a different bundle clock. Why it holds: patches re-marshal through
  sorted `yaml.v3` maps (`patches.go:223`), the walk is lexical, and the machine config takes no
  time-derived material (INFERRED). **The hash inputs** are: repo bytes, secrets bundle, contract,
  machinery version, env vars read by `env` templates, vals-resolved values, and `RuntimeData` when
  templates use it. Identical output across machinery *minor* versions is UNVERIFIED and unlikely.

## 4. Secrets

- **Default** (`pkg/providers/secrets_filesystem.go`): `Get` reads `secretsPath` through SOPS, then
  vals (`:29-32`; `internal/decryption/decryption.go:20-64`). If the `sops` binary is missing, the file
  is treated as plaintext (`internal/sops/sops.go:23-32`). vals runs only if a `ref+x://` exists, and
  then the `vals` binary is required (`internal/vals/vals.go:29-37`). `Put` runs
  `sops encrypt --filename-override <path>`. **If that fails, it silently writes plaintext** with mode
  0600 (`:34-49`; `docs/providers.md:59`). Verified: in `/tmp` with no `.sops.yaml`, the file was
  written in the clear.
- **Provider contract** (`pkg/providers/secrets.go:20-27`, `secrets_binary.go:26,39`):
  `<bin> secrets get <cluster>` writes the bundle YAML to stdout. Empty stdout means "not found".
  `<bin> secrets put <cluster>` reads the bundle on stdin. A non-zero exit is an error; stderr passes
  through (`docs/providers.md:61-86`). There is no list, delete or rotate verb. The loaded bundle is
  checked with `bundle.Validate(nil)` against the *current* Talos version (`secrets.go:50`). Lab
  check: one `get`, then one `put` on first run (`provlab/calls.log`).
- **Generation**: lazily, on first `Secrets()` call. If the bundle is not found, Topf runs
  `secrets.NewBundle(clock, nil)` and then `Put` (`internal/topf/secrets.go:33-34,48-80`). With
  `--confirm` (the default) it prompts first. **`render` (offline) also generates and stores a new
  bundle** if none exists (verified in `provlab`). `topf secrets` prints the bundle unredacted
  (`docs/commands/secrets.md`).
- **Rotation**: no command and no docs. Rotation appears only in a redaction comment: the old keys on
  the node are masked (`internal/topf/current_config_secrets.go:13-16`, `nodes.go:80-98`). Rotation is
  out of scope for Topf (INFERRED).

## 5. Nodes provider contract

- Call: `<bin> nodes <clusterName>`, with no other arguments, env or stdin (`pkg/providers/nodes_binary.go:25`).
  stdout must be a YAML **list of `Node`** with the same schema as `nodes[]` (§2), so the same
  validation applies (`host` and `role` required). Exit 0 means success; stderr passes through
  (`docs/providers.md:11-36`).
- Merge: provider nodes are **appended** to the static `nodes[]`, then sorted (`topf_config.go:106-131`).
  There is **no de-duplication**, so a host listed in both places appears twice (INFERRED from the
  code). `--nodes-filter` applies afterwards (`providers.md:38-40`).
- It runs **inside `config.LoadFromFile`** (`topf_config.go:106-120`), so every command calls it,
  including `render`.
- `data` reaches templates as `.Node.Data.<key>` (`providers.md:36`). Lab check: `data.uuid` rendered
  into a node label. Topf has **no native UUID field**. `data.uuid` is only the convention the docs
  example uses.

## 6. Delivery: Topf pushes, the service pulls

- **apply** (`internal/cmd/apply/apply.go:47-79`, `internal/topf/apply.go:25-98`): Topf collects live
  info over COSI, runs pre-flight checks (ready, stage in Running/Maintenance/Booting, `:100`), then
  calls `ApplyConfiguration` in two steps. First a dry run, and if it reports "No changes" it stops.
  Otherwise it applies with `--mode` auto, reboot, no-reboot, staged or try. Control planes go one at a
  time; workers go in a rolling pool (`apply.go:142-158`).
  **Maintenance mode is detected automatically.** Topf probes TCP 50000. If the server asks for no
  client certificate, Topf uses an `InsecureSkipVerify` client, the equivalent of `--insecure`
  (`client.go:24-40,81-96,101-132`). Otherwise it mints an mTLS client from the bundle (`:43-77`).
- **bootstrap**: `apply --auto-bootstrap` targets the first control plane. It retries etcd
  `Bootstrap` for 10 minutes when etcd is Preparing or Waiting (`bootstrap.go:20-78`).
- **upgrade**: a node is upgraded if its running version or schematic differs from the generated
  installer image (`upgrade.go:279`). It uses the LifecycleService on 1.13 and later, and legacy
  `Upgrade` below that (`:312-332`). Kubernetes upgrades go through `talosctl upgrade-k8s`, or bump
  `kubernetesVersion` and `apply` with no skew checks (`docs/kubernetes-upgrade.md`).
- **reset**: wipes `STATE` and `EPHEMERAL` (or everything with `--full`), graceful, then reboots
  (`internal/cmd/reset/reset.go:75-100`).

**Coexistence.** Talos uses a persisted `STATE` config before `talos.config=` (sib:04 `:209-212`).
So the service's pull path only runs on first install or after a wipe. Topf's push path covers day 2.
- *Natural handoff*: `topf reset`, then the node netboots, then the service serves `/config?u=`.
  Reinstall stays on the D13 path.
- *Drift 1: contract.* `apply` renders with the **running** version. The service has to pick the
  contract at PXE time, and the booted image version is the equivalent. If the PXE image differs from
  `talosVersion`, the bytes differ.
- *Drift 2: the approval gate is bypassed.* A `topf apply` from CI pushes repo revision N without the
  service ever approving it. On the next reinstall the service renders N. N's hash differs from the
  hash approved at N-1, so the node blocks per D14 even though it already runs N. INFERRED.
- *Drift 3: inputs the service cannot see*: `env` templates, `RuntimeData` branches, vals lookups at a
  different time, and a different machinery version.
- *Mitigation*: make approval a repo-revision event. The service renders every member at revision N
  and shows the diff. The owner approves and the hashes are stored. Only then does CI run
  `topf apply`. Or CI posts the applied revision to the service. INFERRED design.

## 7. Is Topf usable as a Go library?

- Public: `pkg/config` (TopfConfig, Node, LoadFromFile, PatchContext, RenderTemplate), `pkg/providers`
  (interfaces, binary and filesystem providers, `LoadSecretsBundle`) and `pkg/clusterinfo`.
  Internal: the runtime and render path (`internal/topf`), decryption, sops, vals, schematic, apply,
  upgrade and reset (file list).
- What external code can use: `config.LoadFromFile` (but it execs `sops` and the nodes provider),
  `LoadSecretsBundle`, `RenderTemplate`, and `PatchContext.Load` **only when every patch is `.tpl`**.
  A nil cache panics (verified). The `@schematic` resolver and the installer-patch logic are
  internal and must be copied.
- **Byte equality is achievable but not supported.** My probe (`probe/main.go`, about 90 lines)
  copies `generateNodeConfig` plus `installerImagePatch` and matched `topf render` byte for byte.
  It needed every patch renamed to `.tpl`. Its only Topf imports are `pkg/config` and `pkg/providers`.
- The options, and what each implies:
  1. **Vendor-copy (MIT, with attribution)** about 300 lines: the render path, decryption, sops, vals
     and schematic. Pin machinery to Topf's release. Add a CI golden test that diffs against the pinned
     `topf render`. Cheapest and in-process. Drift risk: upstream changes defaults silently.
  2. **Fork** and move the render path to `pkg/`. Clean API, but you own the fork. Offering it upstream
     needs the owner's explicit permission.
  3. **Exec `topf render`**: byte-equal by construction and the same binary as CI. It writes plaintext
     configs to a directory and selects the contract only through `topf.yaml`. Offline it cannot pick
     a contract per node. It costs a process per request, and it auto-generates secrets if they are
     missing.
  Avoid `go get …@v0.6.2` in any case: the proxy content differs from the release (finding 4). Pin a
  commit pseudo-version.

## 8. Integration options

| | (a) service is Topf's nodes/secrets provider | (b) service imports a Topf repo, renders on request | (c) service exports a Topf repo it authored | (d) b + a(nodes, secrets-get) |
|---|---|---|---|---|
| What | A shim binary answers `nodes <c>` (approved hosts, `data.uuid`) and `secrets get/put` via the service API | git sync of `topf.yaml`, patches and secrets ref; per-UUID render at `/config?u=` | writes `topf.yaml`, patches and `secretsProvider:` pointing at the shim | home-ops imported; Topf CLI kept for day 2 |
| D13 pull | none. Topf still pushes. Unknown hosts get no config | **fits**: UUID maps to `data.uuid` maps to host, render, serve | fits for authored clusters; service renders natively | fits |
| D14 hash | none; Topf applies outside the gate | **fits**: deterministic (§3); store the hash per member and revision | hash of the service's own render | fits if approval is a revision event (§6) |
| D12 seam | `get` reads the seam. `put` needs a writable sink, and env/file are not writable, so the shim must refuse `put`, which is safe | service reads the bundle from the seam; never reads `secrets.yaml` | same | same |
| Pros | small shim; reuses Topf upgrade/reset/bootstrap; inventory single-sourced | one source of truth (git); the service's output equals Topf's | escape hatch; operators can use Topf | covers import *and* day 2 |
| Cons | the CA keys leave the service for every CI run (§0.5); no pull | must vendor or fork the internal render (§7); `nodes[].data.uuid` must be added | YAGNI today; Topf schema churn (0.x) | the most moving parts; two writers to nodes |

## 9. home-ops migration (structure only, `origin/main` = `c8a370ccf`)

`talos/` holds: `talconfig.yaml`, `talenv.yaml` (plaintext, keys `domainName`, `clusterName`,
`clusterEndpointVIP`), `talsecret.sops.yaml` (top-level `cluster`, `secrets`, `trustdinfo`, `certs`
and `sops`, the standard bundle shape), `patches/{global,controlplane,worker,node/<short-host>}/`
(26 YAML patches plus a README), `mod.just` (talhelper `genconfig` and `gencommand apply|upgrade`), `netboot/ms-r1/`, and
`clusterconfig/.gitignore`. There are 3 control planes (talos-01-cp, talos-03-cp, r230-t02) and 2
workers (talos-04-wrk, talos-05-wrk arm64). Pinned to `talosVersion v1.12.11` and
`kubernetesVersion v1.35.10`.

| talhelper construct in use | Topf equivalent (source) |
|---|---|
| `clusterName: ${clusterName}`, `endpoint: …${domainName}` | literal values in `topf.yaml`. It is not templated (§2), so envsubst has no equivalent there |
| `talosVersion`, `kubernetesVersion` | same keys. `talosVersion` becomes **the contract**. Stay on v1.12.11 for the migration (SKILL.md:22-32) |
| `domain`, `clusterPodNets/SvcNets`, `cniConfig: none`, cert SANs (with `${…}`) | `all/` patches; SANs in a `.tpl` using `.Data` (field-mapping.md:22-29; migration doc :123-152) |
| `talenv.yaml` substitutions (`${clusterEndpointVIP}` in node VIPs and `global/static-host-entries.yaml`) | `data:` in `topf.yaml` plus `.yaml.tpl` patches with `{{ .Data.x }}` (field-mapping.md:98) |
| global `patches:` (9 `@./patches/global/*`) | move to `all/`. **`global/containerd.yaml` is present but not referenced**, so in `all/` it would become active. Decide per file |
| `controlPlane:`/`worker:` `patches` and `nodeLabels` | `control-plane/` (dir renamed from `controlplane/`) and `worker/` patches (field-mapping.md:40) |
| node `patches` (`@./patches/node/<short>/…`) | `node/<host>/`. `<host>` must equal `nodes[].host`. talhelper hostnames are FQDNs built with `${…}`, so either use short `host` plus a `HostnameConfig` `.tpl`, or rename the dirs (SKILL.md:470-486) |
| `hostname`/`ipAddress`/`controlPlane` | `host`/`ip`/`role` (migration doc :18-58) |
| `installDisk`, `installDiskSelector` (r230-t02) | node patch `machine.install.disk`/`diskSelector` on the 1.12 contract; `UnattendedInstallConfig` after 1.14. Otherwise Topf's default is `/dev/sda` (§2) |
| `networkInterfaces` (bond, VLAN, `vip`, `ignore`) | node patch `machine.network.interfaces`; `vip` via `.tpl` |
| `userVolumes`, `volumes` (EPHEMERAL) | node patches. field-mapping.md:72-73 says `machine.userVolumes`/`machine.volumes`. UNVERIFIED: Talos models these as `UserVolumeConfig`/`VolumeConfig` documents, so check before trusting the mapping |
| node-level `nodeLabels` replacing the group's (r230-t02) | Topf merges, it does not replace. The node restates every label, so the result matches (INFERRED) |
| `schematic` per role, replaced per node (all 5 nodes override) | `schematicId: "@schematic.yaml.tpl"` keyed on `.Node.Role`/`.Node.Host`, or per-node `@schematics/<n>.yaml`. Check with `topf schematic-ids` (SKILL.md:400-421) |
| `machineSpec.arch: arm64` | no field; flag it (SKILL.md:510-514). The factory installer is multi-arch (INFERRED) |
| `$patch: delete` on WatchdogTimerConfig (talos-05-wrk) | supported as-is (migration doc :111-121) |
| JSON patches | none in use. The `op: create` lines are `machine.files` entries |
| `talsecret.sops.yaml` | keep it and set `secretsPath: talsecret.sops.yaml`. The `.sops.yaml` rule `.*\.sops\.ya?ml` still matches, so a later `Put` encrypts. Renaming to `secrets.yaml` would match no rule, and Topf would silently write plaintext (§4) |
| `mod.just` gencommand apply/upgrade | `topf apply` / `topf upgrade --nodes-filter <host>` |

## Recommendation for the service

Choose **(d), led by (b)**:
1. **Render in-process** from an imported Topf repo. Vendor-copy the render path (§7 option 1) at a
   pinned Topf commit and machinery version. Add a CI golden test against that `topf render`. Serve
   `/config?u=${uuid}` by mapping UUID to `nodes[].data.uuid` to host. The D14 hash is
   `sha256(rendered bytes)`, keyed by member and repo revision.
2. **Set the contract to the booted Talos image** and require it to equal `topf.yaml talosVersion`
   when approving. Leave `RuntimeData` empty, the same as an offline render.
3. **Run a nodes-provider shim** so `topf upgrade/reset/apply` see the same approved inventory. Ship
   **secrets `get` only if** the owner accepts CA keys in CI. Always refuse `put`. The service, not
   Topf, generates bundles, behind the D12 seam.
4. **Approval is per repo revision** (§6 mitigation). `topf reset` returns a node to the pull path.
5. Defer (c) export (YAGNI). Refuse to render when the secrets bundle is missing. Never call
   `generateAndStoreSecrets`.

## Lab tests

1. Golden parity: copy home-ops to a scratch branch, migrate it, and diff `topf render` (pinned
   commit) against the vendored renderer for all 5 nodes. Also diff against `talhelper genconfig`
   (SKILL.md:360-387).
2. Contract: PXE-boot 1.12.11 while `talosVersion` is 1.12.11, then while it is 1.14.4. Confirm the
   unattended install succeeds and record the hash change.
3. Merge semantics: global and r230-t02 `containerd.yaml` both create
   `/etc/cri/conf.d/20-customization.part`. Check whether the merged `machine.files` duplicates it or
   Talos rejects it (INFERRED risk).
4. Drift loop: `topf apply` revision N without service approval, `topf reset`, PXE. Confirm D14 blocks,
   then approve and confirm it serves.
5. Machinery bump: render the same inputs with machinery 1.14.4 and the next minor. Measure the hash
   churn before any upgrade policy is set.

## Owner decisions raised

1. Can CA private keys reach the Topf runner (CI or a laptop)? Yes enables (a) secrets-get and
   Topf-driven day 2. No means the service must do upgrade and reset itself.
2. Vendor-copy, fork, or exec `topf render`? (§7.) Forking or offering anything upstream needs your
   explicit per-instance permission.
3. Where does the UUID live: `nodes[].data.uuid` in the repo, or the service's approval DB with the
   nodes shim injecting it?
4. Does the service rewrite the contract to the booted image, or force PXE image = `talosVersion`?
5. Do the home-ops migration on 1.12 single-doc first and move to 1.14 multi-doc as a separate change
   (Topf's own advice)? Also decide `global/containerd.yaml`'s fate and short vs FQDN `host`.
