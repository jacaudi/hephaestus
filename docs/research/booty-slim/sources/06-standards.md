# 06 - Standards sources: appkit, react-kit, design-tokens, and their source doc

Researched 2026-11-12, read-only. Citation key (all paths absolute-able under `~/Projects/local/spike/`):

- `WL` = `openspec/docs/workflow-lenses.md` (831 lines; line numbers are file lines).
- `AK` = `appkit/README.md`, `AKB` = `appkit/docs/brief.md`.
- `RK` = `react-kit/README.md`, `RKB` = `react-kit/docs/brief.md`.
- `DT` = `design-tokens/README.md`, `DTD` = `design-tokens/docs/designs/2026-11-10-design-tokens-v1-design.md`.
- `GS` = `~/.config/reference/go-standards.md`, `12F` = `~/.config/reference/12-factor-go-mapping.md`.
- `MEM` = Claude memory under `~/.claude/projects/-Users-$USER-Projects-github-booty/memory/` (unless noted).
- booty code = `~/Projects/github/booty/...`.
- INFERRED = my reading, not stated in a source.

## 1. Source of truth and its status

**Source doc: `WL`, "OpenSpec workflow: three lenses".** Status line: "working draft, 2026-11-09" (`WL:3`).
Sections that matter: Lens 2 (SWE) holds the application standards (`WL:117`, `WL:10-11`). Lens 3 binds them to home-ops (`WL:628`).

How I determined this:
- `AK:18` and `RK:14` both say "This repo implements part of the application standard in `~/Projects/local/spike/openspec/docs/workflow-lenses.md` (not yet committed)".
- `AKB:38-40` names it, "Lens 2 (application standards) and Lens 3 (platform)".
- `DTD:4-5` names it: "Lens 2, 'Application standard: UI', token contract".
- `RKB` does not cite a path. It cites GSDP and the layer model only (`RKB:3-12`). INFERRED: the same source via `RK:14`.

Status of the sections booty cares about (all inside a "working draft" doc):

| Section | Status | Cite |
|---|---|---|
| Application standard: UI (two tiers, app mapping, token contract) | **decided 2026-11-09. "Documented only: no app migration or port is authorized."** The token contract subsection is headed "(draft)". | `WL:160`, `WL:217` |
| Backend and infra standard overall | "in progress"; languages and Go standard decided 2026-11-09 | `WL:260-261` |
| Database | decided 2026-11-09 | `WL:316` |
| API conventions | decided 2026-11-09 | `WL:355` |
| Observability and health (admin port, telemetry, logs, wide events) | decided 2026-11-09 | `WL:374` |
| Authentication | decided 2026-11-09 | `WL:504` |
| Container images | decided 2026-11-09 | `WL:545` |
| Operators | decided 2026-11-10 (not relevant to booty) | `WL:572` |
| Releases | decided 2026-11-10 | `WL:596` |
| Health and admin port wiring (cluster binding) | decided 2026-11-10 | `WL:692` |
| Pod hardening and images (cluster binding) | decided 2026-11-10 | `WL:747` |

**Convergence rule** (`WL:263-265`): "new code follows the standard; existing projects converge when migrations are authorized." It overrides go-standards' "Existing projects: follow their established patterns" for any project whose migration is authorized.
INFERRED: booty is an existing project. The standard binds a slim-booty rewrite only if the owner authorizes it as a migration or treats it as new code.

Where the spikes sit:
- `appkit/` and `react-kit/` each contain only `README.md` and `docs/brief.md`. `git log` says "your current branch 'main' does not have any commits yet".
- `openspec/` also has no commits (`docs/` and `schemas/` untracked). `AK:18` says "not yet committed" of `WL`.
- `design-tokens/` has 27 commits locally and no remote (latest 2026-11-11 21:20 -0800). It has code, tokens and tests.
- Destination for all three: Forgejo org `jacaudi`, push-mirrored to `github.com/jacaudi/*`, released with releaser-pleaser (`AK:5`, `RK:5`, `WL:28`). "For now (owner, 2026-11-10) the three repos start as local spikes" (`WL:28`).
- `DT:5` says `go.jacaudi.dev` does not serve the module yet. Until it does, consume it through a `go.work` workspace.

Build order (decided): `design-tokens` first, then `appkit`. react-kit follows or goes alongside. `DTD:443` says "design-tokens before react-kit and appkit (2026-11-10)". `AKB:3-4` says design-tokens first, "corrected from an earlier 'react-kit first'", and appkit "resumes after".

## 2. appkit

Status: **brief only, recorded 2026-11-10. "Nothing is designed or built yet"** (`AKB:3`, `AK:18`). Contents "follow the reuse-as-code design, which has not been done." Everything below is therefore **PROPOSED scope**, not decided API. The only decided items are the underlying standards it will implement (decided in `WL`, cited per row).

Definition: "the Go module every service imports, so the backend standard is implemented **once** instead of copied into each repo" (`AKB:8-9`). Module path `go.jacaudi.dev/appkit` (decided, `WL:28`).

| # | Component | What the spike says | Underlying standard | Status |
|---|---|---|---|---|
| 1 | Config + HTTP server | Viper, file + env, validated at startup (`AK:9`) | One Viper instance with `ExperimentalBindStruct`, file + env, validated at startup (`WL:308`; `12F:15`) | scope proposed; standard decided |
| 2 | Admin port | `/healthz`, `/readyz`, `/health` (`application/health+json`), `/metrics` (`AK:10`) | See section 6 (`WL:379-429`) | scope proposed; standard decided |
| 3 | Telemetry patch bay + wide events | Patch 0 local always on, patch 1 OTLP optional (`AK:11`) | `WL:431-490` | scope proposed; standard decided |
| 4 | Auth | OIDC, built-in passkey accounts, API keys; recovery-link notifier patch bay (`AK:12`) | See section 5 (`WL:504-543`) | scope proposed; standard decided |
| 5 | Database | SQLite or Postgres from `DATABASE_URL`; bun, goose (`AK:13`) | `WL:316-353`, `WL:673` | scope proposed; standard decided |
| 6 | `GOMEMLIMIT` from cgroup limit | `AK:14` | "set by the shared module from the cgroup limit rather than per app (`automemlimit` is a candidate library, to verify)" (`WL:756`) | scope proposed; library choice open |

Consumers: "Go services in both UI tiers". Operators use only part, because they keep controller-runtime's server (`AKB:18-19`). booty is a service, so a full consumer (INFERRED).

Findings in the brief that shape design (`AKB:21-29`):
- Auth is extracted from two existing implementations, GSDP and diyddns. The named constructors are `NewCompositeAuthenticator`, `NewOIDCAuthenticator`, `NewSessionAuthenticator`, `NewAPIKeyAuthenticator`, `NewPasskeyLinkToken`, `NewKeyring` (GSDP) and `NewOIDCService` (browser + device flows), `NewPasskeyService`, `NewSessionManager`, `NewAPIKeyService`, CSRF, HMAC (diyddns). The same list is in `WL:540-543`.
- The go-standards patterns (graceful shutdown, health-check state machine, secret type, version injection) "are already pasted into several repos" (`AKB:26-27`). `WL:620-621` states the known gap: "reuse as code: the patterns above are copied ... not imported from a shared module".
- **"No named first consumer yet"** (`AKB:28-30`). Building all six areas before an app uses them risks speculative APIs (YAGNI). A first consumer would define the first slice. INFERRED: booty could be that named consumer. Nothing says so.

Process, proposed and undecided (`AKB:31-36`): run appkit's design through the OpenSpec `feature-workflow` schema as its first real use. The alternative is the classic `docs/designs/` path.

Implication for slim-booty: appkit does not exist as importable code. A slim-booty feature list can only say "conforms to the appkit scope" by implementing the `WL` standards directly, ideally in shapes that lift cleanly into appkit later (Patchbay). Do not depend on `go.jacaudi.dev/appkit`.

## 3. react-kit

Status: **brief only, dated 2026-11-10. "Nothing designed or built. design-tokens comes first."** (`RKB:3`, `RK:14`). Everything is PROPOSED.

Definition: "Shared Bun package for heavy-tier (React) frontends" (`RK:3`). Scope (`RK:9-10`):
- antd theme wiring from design-tokens (primary), and shadcn/ui support (secondary).
- API clients generated from each service's committed OpenAPI spec. The underlying standard is decided: "Generated from the committed spec (TypeScript for the heavy tier, Go where needed), never handwritten" (`WL:368`).

Layer model (`RKB:5-12`):
- design-tokens holds how things look (colours, fonts, radius, shadow; later tier-3 per-component styling).
- react-kit holds what things are (reusable React components).
- "react-kit components consume design-tokens roles; they never hold a colour or font literal" (`RKB:12`).

Proposed rule (`RKB:14`): "use antd directly; wrap only when the wrapper adds meaning." The brief flags it as **"To confirm with the owner: what 'meaning' means"** (`RKB:16`). Candidate wrappers, all drawn from GSDP's 14 wrappers (`RKB:20-29`):

| Wrapper | Kind of meaning | Proposed |
|---|---|---|
| `ToneTag` | value to role colour | yes, shared candidate |
| `StatusTag`, `HazardTag` | domain state or palette | yes, stay in GSDP |
| `AppModal` | enforces an app rule (only modal; ESLint `no-restricted-imports` forbids raw antd `Modal`) | yes, shared candidate |
| `QueryBody`, `ListError` | behaviour pattern (skeleton while loading, panel error over last data) | yes, shared candidate |
| `UnsavedChangesBar` | workflow | yes, shared candidate |
| `Panel`, `Tile`, `Section`, `StatCard`, `ListParts` | layout or visual only | **open** |
| `BottomSheet`, `ListDetail` | device layout | **open** |

Not meaning (proposed): renaming, re-exporting, or passing props through unchanged (`RKB:31`).
Open (`RKB:40-44`): confirm the categories; v1 scope (earlier recommendation was option A, "the API-client layer", with wrappers later); first shared candidates would be `ToneTag`, `AppModal`, `QueryBody`/`ListError`.

Relevant to booty:
- The generated OpenAPI client is the one decided standard (`WL:368`) a React booty would consume. booty already runs Huma and serves OpenAPI (see section 5/6).
- The wrapper rule echoes booty's own memory rule: "use Ant Design components exclusively ... Do not build custom UI components without explicit user approval" (`~/.claude/projects/-Users-$USER-Projects-booty/memory/feedback_ui_framework.md:7-9`). react-kit's "wrap only when it adds meaning" is compatible but more permissive (INFERRED).
- react-kit is Bun-based (`RK:3`). booty's `web/` uses npm and `package-lock.json` (`booty/web/package.json`, taskfile `web` task runs `npm ci`). That would converge to Bun under `WL:274` and `WL:552`.
- If booty moves to the light tier, react-kit is irrelevant to it (INFERRED).

## 4. design-tokens

Status: **the only spike with real code.** `DTD:3`: "Revision 4 (2026-11-11) ... Status: approved and implemented". Local spike, v1 targets GSDP/antd (`DT:5`). Module `go.jacaudi.dev/design-tokens` (`design-tokens/go.mod:1`; decided `DTD:54`, `WL:28`). Contents verified present: `tokens/{roles.json, primitives, base, sets/gsdp, domain, resolver}`, `cmd/tokens`, `internal/{dtcg,resolve,color,check,gen/antd,domain,refresh}`, `taskfile.yml`.

### 4.1 Decided in the standard (`WL:217-249`)

- Pick a role, never a value. Components use semantic roles (`color.action.primary`, `feedback.warning`); lint rejects raw hex and arbitrary values (`WL:219-220`).
- Three tiers: primitive, semantic, component. Agents use only the last two (`WL:221`).
- Three axes per project theme: mode (light, dark), palette (named set, e.g. `nord`, `tokyo-night`), style (`flat`, `glass`) (`WL:222-228`).
- Feedback roles shared and locked: `warning`, `danger`, `success`, `info`. A project may tune the shade only within the hue family and only if contrast passes (`WL:230-232`).
- Domain palettes in their own namespace with source URL and revision, used only for data and always with a legend (`WL:233-236`).
- Format: W3C Design Tokens Format 2025.10, `*.tokens.json`, no `tokens/` folder convention in consumers. Shipped as a Go module that embeds the built CSS and antd theme, carries domain palettes as Go constants, and includes the build-and-check command (`WL:237-239`).
- Contract test: every role in every declared mode; WCAG 2.2 AA pairs (4.5:1 text, 3:1 large text and UI parts); feedback colours stay in hue family (`WL:240-241`).
- Enforcement: lint (no raw colour or arbitrary Tailwind values outside token files), axe under Playwright in every mode, a `ui-standards` capability whose scenarios carry `Check:` lines (`WL:251-256`).
- Token output per tier: light tier gets shadcn CSS variable names; heavy tier gets "antd theme config, plus shadcn variables for the secondary" (`WL:180`).

### 4.2 What v1 actually does (`DTD`, `DT`)

- One output: `tokens build --target antd --set gsdp --out <path>`, producing a generated TypeScript file (`DTD:22`, `DTD:308-310`, `DT:9`, `DT:14-17`).
- One token set: `gsdp`. Five domain palettes: NWS hazards, SPC outlooks, LSR, SPC MD, mPING (`DT:9`).
- Not in v1: the shadcn CSS-variable output (light tier), the `glass` style, per-app theme builds, adopting the output in GSDP (`DTD:32-39`). The light-tier shadcn generator is "added when a light-tier consumer is authorized" (`DTD:32`).
- CLI: `check [--json]` (contract rules 1-4 and 6, exits 1 on a finding), `build`, `refresh-domain`, `version` (`DT:19`, `DTD:97`, `DTD:364`). Usage errors exit 2.
- Output shape (`DTD:312-339`): header "Code generated by design-tokens ... DO NOT EDIT" plus a source hash; imports `theme, MappingAlgorithm` from `antd`; exports `lightStep`, `darkStep`, `themeToken(mode)` returning `{ algorithm: [theme.darkAlgorithm|defaultAlgorithm, step], token }`, per-role const records (e.g. `actionPrimaryTextHover`), `tones`, `nwsHazards`, `spcOutlooks`.
- Pins: GSDP uses antd 6.6.6 and @ant-design/colors 8.0.1 (`DTD:6-7`). Primitives equal to @ant-design/colors presets are named `palette.<hue>.<1-10>` (`DTD:144-149`).
- No Node needed to build or consume the module (`DTD:48`, `DTD:61`).

### 4.3 How a React + antd app consumes it (`DTD:344-353`, `DT:11-17`)

1. Pin the CLI with a Go tool directive: `go get -tool go.jacaudi.dev/design-tokens/cmd/tokens@v1.x.y` (adds `require` and `tool` lines to `go.mod`).
2. A task runs `go tool tokens build --target antd --set <set> --out web/src/theme/tokens.gen.ts`.
3. Commit the generated output. The app imports `themeToken(mode)` into antd `ConfigProvider` in place of hand-maintained constants.
4. Until `go.jacaudi.dev` serves the module, use a `go.work` workspace (`DT:5`; `DTD:56-57`: a committed local `replace` is forbidden by go-standards).
5. GSDP keeps its `components` block (Layout, Switch, Tabs, ...), `ThemeProvider`, mode toggle and breakpoints locally, built from the generated constants (`DTD:35-38`).

The generate-into-the-app model (not an npm package) is a recorded call "owner may revise" (`DTD:456`). It diverges from the standard's "embedded in the Go module" (`DTD:427-429`).

### 4.4 Gaps relevant to booty (INFERRED from the above)

- **No `booty` set exists.** Only `gsdp` (`DTD:446`). The contract says named sets can be added, but v1 is "GSDP-exact" and does not split or add sets (`DTD:14-15`, `DTD:349-350`). A booty set means a new `sets/booty/` plus resolver. That is v-next work in the design-tokens repo, not a booty-side task.
- **antd version mismatch.** booty is on `antd ^5.21` (`booty/web/package.json`; memory `booty-ui-ant-design.md:12`). design-tokens is built and golden-tested against antd 6.6.6 (`DTD:6-7`). A v5-to-v6 upgrade is already scheduled for booty "P7" (`booty-ui-ant-design.md:14`). INFERRED: the generated file's `theme.darkAlgorithm` / `MappingAlgorithm` shape probably works on v5, but v6 token behaviour is what was verified (`DTD:303-306`). Not tested on v5.
- **If booty goes light tier** (`WL:211`), the needed output is the shadcn CSS-variable generator, which is explicitly not built (`DTD:32`).
- booty's UI rule "tokens, never hex ... ConfigProvider-only theming" (`booty-ui-ant-design.md:18`) already matches "pick a role, never a value" in spirit.

## 5. Auth standard, booty today, gap list

### 5.1 Required (decided 2026-11-09, `WL:504-543`)

- Users authenticate **only** with OIDC or passkeys. "No passwords anywhere ... and no other user-facing method." (`WL:506-507`).

| Who | Mechanism | Cite |
|---|---|---|
| Users with an IdP | OIDC authorization code + PKCE against any compliant IdP. Authentik at `auth.jacaudi.dev` is home but optional | `WL:511` |
| Users without an IdP | Built-in passkey accounts (WebAuthn), "a local mode every app supports" | `WL:512` |
| Users on a CLI | OIDC device flow when an IdP is configured; otherwise a personal API key | `WL:513` |
| Machines | API keys, "wherever machine access is needed": app-issued, random, stored hashed, scoped, expiring, revocable, shown once, sent as `Authorization: Bearer` | `WL:514` |
| Third-party apps without a user model | Gateway forward-auth; "Apps built here never trust identity headers" | `WL:515` |

Calls set in the doc (`WL:517-538`):
- **Sessions** belong to the app: a secure, `HttpOnly`, `SameSite` cookie. Authorization uses the IdP groups claim in OIDC mode and app-local roles in passkey mode (`WL:519-520`).
- **Forms carry CSRF protection**; htmx sends the token in a request header (`WL:521`).
- **First account in local mode** is enrolled with a one-time bootstrap token from configuration, "never 'first visitor becomes admin'" (`WL:522-523`).
- **Recovery** in local mode is an admin-issued, time-limited re-enrollment link; users are encouraged to register at least two passkeys. Both modes enabled at once need explicit account linking (GSDP's link-token pattern) (`WL:524-525`).
- **Automated delivery of recovery/enrollment links** (email, Signal, ...) is allowed (owner decision) as an isolated notifier patch bay behind one interface. Rules (`WL:526-538`):
  - single-use, short-lived, bound to one account, never logged;
  - sent only to a destination verified at enrollment;
  - rate-limited per account and destination, every issue and redemption recorded;
  - a failed delivery never reveals whether the account exists;
  - the admin-issued link stays as the fallback;
  - apps state which accounts allow self-service.
- **Reuse:** one shared `auth` package extracted from GSDP and diyddns, "not a third implementation" (`WL:540-543`). That package is appkit's auth area (`AK:12`).
- **Cluster binding:** an OIDC client per app from the IdP. IdP choice open (Authentik or Pocket ID with an operator; kaniop under test) (`WL:656`, `WL:799`). "Apps are unaffected by the choice" (`WL:804`).

### 5.2 What booty ships today (code at `~/Projects/github/booty`, HEAD `c74c686` on this branch's base; memory dated 2026-08-30)

- **One shared static API token**, not per-user accounts. `pkg/auth/token.go:1-3`: "booty's single shared API token and the stateless session cookie derived from it".
  - Token is auto-generated on first run and persisted at `<dataDir>/api-token`, or supplied by `--apiToken` / `BOOTY_API_TOKEN` (`cmd/main.go:153,402-419`; `pkg/config/config.go:128`).
  - Empty-token guard (`ErrEmptyToken`) closes a constant-key cookie forgery (`pkg/auth/token.go:36-41`).
  - `Secret` type redacts the token (`token.go:45-49`).
- **API auth** is `X-Booty-Token` header OR session cookie, on the whole `/api/v1` huma group, via `authMiddleware` (`pkg/http/auth.go:14-44`). It returns 401 `application/problem+json` (`auth.go:38-42`). A test walks the OpenAPI doc to prove every operation is gated (`auth.go:25-30`).
- **Session** is a `booty_session` cookie: signed `<expiry>.<HMAC-SHA256>`, fixed 7-day lifetime, `HttpOnly`, `SameSite=Strict`, `Secure` when the browser connection is TLS (`pkg/auth/cookie.go:12-32`; `pkg/http/auth.go:112-130`).
- **Login** is `POST /login` with a pasted token (`auth.go:95-103`); `/logout` requires a valid session to prevent cross-site logout (`auth.go:150-163`). There is a `--noAuth` escape hatch, logged loudly (`cmd/main.go:389`).
- **No passkeys, OIDC or user accounts:** `grep -rli "webauthn|passkey|oidc|openid"` over `pkg`, `cmd`, `web/src` and `go.mod` returned zero files. No CSRF token (relies on `SameSite=Strict`, with the reasoning in `auth.go:153-161`).
- Open boot routes (`/ignition.json`, `/machineconfig`, `/booty.ipxe`, `/menu/`, `/node/report`, `/healthz`, `/data/` subtrees) are deliberately unauthenticated for machine clients. They use a boot-ticket scheme (`pkg/bootticket`; `pkg/http/http.go:53-63`). That is a separate concern from user auth.
- The memory records three parked, unresolved operator decisions that touch the open routes: 302 vs 410 for retired routes, client-asserted MAC identity on `/machineconfig`, and unbounded `unknownHosts` (`MEM/booty-auth-design.md:96-106`). The memory note says the auth PR #86 was open on 2026-08-30. The code is present at HEAD (`c74c686`, v0.4.1), so it has merged (INFERRED from the files existing).

### 5.3 Gap list (booty today vs standard)

| # | Standard requires | booty today | Gap |
|---|---|---|---|
| G1 | OIDC auth code + PKCE (`WL:511`) | none | missing |
| G2 | Built-in passkey accounts, WebAuthn (`WL:512`) | none; shared token paste-login | missing |
| G3 | "No passwords anywhere" (`WL:506`) | A shared bearer token pasted into a form is not a password per se, but it is not OIDC or a passkey either (INFERRED) | non-conforming user-facing method |
| G4 | Machine API keys: hashed, scoped, expiring, revocable, shown once, `Authorization: Bearer` (`WL:514`) | one shared token, header `X-Booty-Token`; stored in plaintext file `api-token`; no scope, expiry or revocation beyond SIGHUP rotate (`cmd/main.go:709-713`) | partly; header name and model differ |
| G5 | CLI: OIDC device flow, or personal API key (`WL:513`) | token header | missing |
| G6 | Authorization via IdP groups or app-local roles (`WL:519-520`) | no roles; a token is all-or-nothing | missing |
| G7 | First-account bootstrap token from config (`WL:522`) | n/a, no accounts. The auto-generated token is only logged once at first run (`cmd/main.go:416`) | n/a until accounts exist |
| G8 | Admin-issued recovery link, 2 passkeys encouraged, optional notifier patch bay (`WL:524-538`) | none | missing |
| G9 | CSRF on forms (`WL:521`) | SameSite=Strict cookie only; JSON API | likely acceptable for a JSON API (INFERRED); a light-tier form UI needs a CSRF token |
| G10 | Session cookie secure, HttpOnly, SameSite (`WL:519`) | met (`auth.go:117-118`, Secure conditional on TLS) | met |
| G11 | Problem+json errors (`WL:364`) | 401 via huma problem+json (`auth.go:38-42`); `/login` uses a plain JSON error (`writeJSONError`) | `/login` and the open-route errors are not problem+json (INFERRED, check `/login` body) |
| G12 | Never trust identity headers (`WL:515`) | booty does not (token or cookie only) | met |

Most of G1, G2, G4, G5 and G8 is exactly appkit's auth scope (`AK:12`, `AKB:14`). Because appkit is unbuilt, slim-booty either builds them in booty first (and lets them become appkit's first slice, per `AKB:28-30`) or defers. That is an owner decision (section 7).

## 6. Backend operational standards booty must meet (decided, `WL`)

### 6.1 Admin port, health, metrics (`WL:379-429`, `WL:692-701`)

- A **second `http.Server` on a separate admin listener** serving only `/healthz`, `/readyz`, `/health`, `/metrics` (`WL:379-381`). It must not be a Host-header-distinguished route on the app server (`WL:380-381`). Not a public route.
- Cluster binding: container port named `admin`, number `9090`, a separate `<app>-admin` ClusterIP Service, probes on `admin`, a serviceMonitor, one CiliumNetworkPolicy (`WL:696-700`).
- One registry of checks feeds all three endpoints. Each check is critical (can fail `/readyz`) or informational (only `warn` on `/health`). Checks run in the background and cache results (`WL:387-389`).

| Endpoint | Shows | Codes | Cite |
|---|---|---|---|
| `/healthz` | `ok`; no dependency checks | 200/503 | `WL:393` |
| `/readyz` | `ok` or failing critical checks; `?verbose` | 200/503 | `WL:394` |
| `/health` | `application/health+json` (`draft-inadarei-api-health-check-06`): `status` pass/warn/fail, `version`, `releaseId`, `serviceId`, `checks` | pass/warn 200, fail 503 | `WL:395` |

- `/ping`, `/ready`, `/livez` are not allowed (`WL:397`).
- `/readyz` fails when: starting (migrations not applied, caches cold), draining after SIGTERM, a required dependency failing, or at capacity (hysteresis). Single-replica and SQLite deployments do not fail readiness for capacity (`WL:400-413`; `GS:724-730` health state machine).
- `/health` never includes secrets and sanitizes errors (`WL:419`).
- Minimum reported (`WL:421-423`): version and git commit, uptime, database ping time, "migrations applied = migrations embedded", disk usage when SQLite, each declared external dependency.
- Metrics: OTel SDK with a Prometheus exporter at `/metrics` on the admin port. Required: request rate, errors and duration, a build-info metric with the version, Go runtime metrics (`WL:457-460`; `GS:853`).

**booty today:** a single `GET /healthz` route on the app mux (`pkg/http/http.go:63`); no admin port, no `/readyz`, no `/health`, no `/metrics`. grep for `/readyz|/metrics|otel|prometheus` in `pkg` and `cmd` found nothing beyond the `/healthz` route. `pkg/db/health.go` exists (DB-level health, not inspected). Gap: whole admin port and metrics.

### 6.2 Logging and telemetry (`WL:462-490`)

- `log/slog`. Default format `json`, `text` for dev. Config `<PREFIX>_LOG_FORMAT`, `<PREFIX>_LOG_LEVEL`, `--log-format`. Fields: `time level msg service version`; errors under key `error`; ids `<thing>_id`; request fields `method path status duration_ms`; `trace_id` when present; **snake_case for every log and event field** (`WL:465-474`).
- **Wide events:** one JSON line per unit of work on stdout, same stream as logs (patch 0, always on). Optional OTLP patch 1 via `<PREFIX>_TELEMETRY_OTLP=true` (`WL:431-451`, `WL:478-485`). `GS:854` ("one span per inbound request") is superseded for new code (`WL:485`).
- Logs go to stdout (`12F:23`).

**booty today:** `slog` yes, but a `TextHandler` to **stderr** only, level from a debug flag (`cmd/logging.go:22-23`), logging uses key `err` (e.g. `cmd/main.go:466,601`) where the standard says `error`. No JSON mode, no wide events, no trace ids. Gap: format, stream, key naming, wide events.

### 6.3 Config (`WL:308`; `12F:15,40`; `GS` toolchain table)

- One Viper instance, `viper.NewWithOptions(viper.ExperimentalBindStruct())`, file + env, typed struct, `SetEnvPrefix`, validated at startup; no global `viper.*` functions (`12F:62`).
- **booty today:** uses global viper (`viper.GetString(config.DataDir)` in `pkg/http/http.go:119`, `viper.BindEnv` in `pkg/config/config.go:122-128`) with Cobra flags and a few `BindEnv` calls (`BOOTY_API_TOKEN`, `DATABASE_PATH`). This conflicts with the standard's no-global-viper rule. Gap.

### 6.4 Database (`WL:316-353`, `WL:673`)

- SQLite (`modernc.org/sqlite`) or Postgres (`pgx/v5`) chosen at deploy time via one setting `<PREFIX>_DATABASE_URL` (`postgres://` or `sqlite:///`). Access via **bun** with `sqlitedialect` and `pgdialect`; migrations via **goose v3 Provider API**, two embedded sets (`migrations/sqlite/`, `migrations/postgres/`) with identical version numbers. Mandatory rules: integration tests against both real DBs, store-package coverage gate, tests apply real migrations, SQLite tables `STRICT`. SQLite means single replica (`WL:318-346`).
- **booty today:** `modernc.org/sqlite` (`go.mod`), `database/sql` plus a hand-rolled migrator over embedded `migrations/0001_init.sql ...` using `PRAGMA user_version` (`pkg/db/migrate.go`). No bun, no goose, no Postgres, setting is `DATABASE_PATH` not `DATABASE_URL`. Gap: access layer, migration tool, Postgres path, `DATABASE_URL`. (The go.mod grep for `uptrace/bun|goose` matched nothing.)

### 6.5 HTTP and API conventions (`WL:306-307`, `WL:355-373`)

- Huma (OpenAPI 3.1); Scalar docs at `/docs`; `/api/v{major}/`; **snake_case** JSON; RFC 9457 problem+json; cursor pagination (`limit`/`cursor`/`next_cursor`); RFC 3339 UTC; committed spec with `oasdiff breaking` in CI; generated clients.
- **booty today:** Huma v2.38 (`go.mod`), `/api/v1`, problem+json via huma (`auth.go:38-42`). JSON is **camelCase** (`json:"firstSeen"`, `json:"lastSeen"` in `pkg/http/api_hosts.go:34-35`). `WL:371` already notes "booty is camelCase". Pagination, committed spec and `oasdiff` not checked. Gap: field casing (a breaking change needing a `/api/v2` per `WL:362`), possibly pagination and spec commit.

### 6.6 Build, task runner, containers (`WL:313`, `WL:545-560`; `GS` 2026-09-27 changelog, `GS:977`)

- Build is **Task with lowercase `taskfile.yml`**; `task ci` runs everything CI runs; `build`, `test`, `lint` tasks (`GS` toolchain table). The 2026-09-27 go-tricorder alignment "taskfile contract replaces Makefile" (`GS:977`). Linting is golangci-lint plus the go-tricorder plugin.
- Images: multi-stage; final `gcr.io/distroless/static-debian13:nonroot`; numeric `USER 65532:65532`; every `FROM` pinned `tag@sha256:`; `CGO_ENABLED=0 -trimpath -ldflags "-s -w -X ..."`; amd64 and arm64; OCI labels; **omit HEALTHCHECK**; SBOM, provenance and cosign on release; registry `ghcr.io/jacaudi/<app>` (`WL:549-560`). **JS builder is Bun**, `bun install --frozen-lockfile`, build output embedded in the Go binary (`WL:552`).
- Pod hardening and `GOMEMLIMIT` at about 85% of the memory limit (`WL:756`; `GS:482`); admin port 9090 (`WL:696`).
- **booty today:** lowercase `taskfile.yml` with `web`, `build`, `test`, `vet`, `fmt`, `lint:bin` and go-tricorder wiring (`booty/taskfile.yml:1-80`). I did not verify a `ci` task (taskfile read to line 80 only). Build flags match (`CGO_ENABLED=0`, `-trimpath`, version `-ldflags`). Dockerfile: `node:lts-alpine` web stage (not Bun), `golang:1.26.9-alpine`, final `gcr.io/distroless/static-debian13` **without `:nonroot` and with no `USER` line** (`Dockerfile:3,11,37`), EXPOSE `8080/tcp 69/udp`, no digest pinning visible, no `HEALTHCHECK` (conforms). `.github/workflows` has `go-test.yml` and `web-test.yml` only. Gap: nonroot user, digest pins, Bun, SBOM/cosign, no `GOMEMLIMIT` in repo.

### 6.7 Graceful shutdown, 12-factor (`12F:21`, `GS:627`)

`signal.NotifyContext`, drain, `Server.Shutdown`. booty has ordered shutdown (`cmd/main.go:599-632`, "shutdown signal received", HTTP and TFTP steps). Partly met; readiness-before-drain needs `/readyz`.

## 7. Open decisions and contradictions

1. **Does booty stay React + antd?** Sources disagree. They are not contradictory in intent but they are not reconciled.
   - **For light tier / htmx:** the approved app mapping (2026-11-09) lists `booty | React + antd | Light | Forms and plain tables only` (`WL:211`). Heavy tier is for "many simultaneous browser-side interactions, browser-side grids, or content used offline" (`WL:179`). A 2026-11-09 measurement (memini `0bb1f0ff-2692-4e77-9c66-8db1fcfff37e`, booty @19eda9d) found 85 `Form.Item`, 21 `rules=`, and four Tables with no sorting, filtering, virtualisation or rowSelection and `pagination={false}`: "form-driven CRUD, a natural htmx fit". I found no separate design doc for this; only the standard row and this memory entry.
   - **For React + antd:** booty memory decisions: "booty's web UI goes to Ant Design" (2026-07-01, `booty-ui-ant-design.md:10`); "use Ant Design components exclusively. Do not build custom UI components without explicit user approval" (`feedback_ui_framework.md:7-9`); a v5-to-v6 upgrade is scheduled for P7 (`booty-ui-ant-design.md:14`); no-emoji rule uses AntD icons and Tags (`booty-ui-no-emoji.md:14`). All pre-date 2026-11-09. The "antd only" rule is also inconsistent with the light tier's component sets (shadcn-templ primary, templ-components secondary, `WL:173-174`). INFERRED: a light-tier move would supersede those memory rules but the owner has not said so.
   - **Authorization gap:** the whole UI standard is "Documented only - no app migration or port is authorized" (`WL:160`). So the light-tier mapping for booty is a recorded direction, not an approved migration.
   - Also note the "appkit/react-kit/design-tokens" asks assume React: react-kit and design-tokens' only built output serve the heavy/antd tier. A light-tier booty would depend on design-tokens' unbuilt shadcn generator (`DTD:32`) and would not use react-kit at all (INFERRED).
2. **Primary vs secondary components swapped.** The memini entry `8a1cd8bb-7c8c-4e36-8b08-77923fe8ab2b` says the owner swapped primary and secondary on 2026-11-09: heavy tier primary is now antd, secondary shadcn/ui. `WL:173-174` and `RK:9` ("antd theme wiring ... (primary) and shadcn/ui support (secondary)") agree with the swap. Older text elsewhere may not.
3. **Module path / import mechanism.** `go.jacaudi.dev/<module>` is decided (`WL:28`, `DTD:54`) but `go.jacaudi.dev` does not serve it yet (`DT:5`). booty's own module is `github.com/jeefy/booty` (`go.mod:1`), a fork lineage path, and `docs/standards.md` says "Module path: TBD". Consuming design-tokens or appkit from booty needs `go.work` (no committed `replace`). Whether booty should rename its module is not addressed (INFERRED open).
4. **Spike maturity.** appkit and react-kit are briefs with no commits and no code; design-tokens is implemented locally with no remote; `WL` is an uncommitted working draft (`AK:18`). "Conform to appkit" cannot be checked mechanically today. The only enforceable text is `WL` itself plus go-standards.
5. **appkit has no first consumer** (`AKB:28-30`). booty could be it, but no decision exists. Also unresolved: whether appkit's design runs through the `feature-workflow` OpenSpec schema or the classic path (`AKB:31-36`).
6. **go-standards copy is stale.** `~/.config/reference/go-standards.md` (72.7 KB) differs from the canonical go-tricorder copy (75.2 KB) (`WL:623-624`). The canonical location is go-tricorder's `docs/standards/go-standards.md` (`WL:296`). Some rows here (e.g. `GS:854` one span per request) are superseded (`WL:485`).
7. **Task runner contradiction (resolved).** Older go-standards text references Makefile. The 2026-09-27 changelog entry says the taskfile contract replaces it (`GS:977`). booty already uses a lowercase `taskfile.yml`.
8. **Convergence vs migration authorization.** `WL:263-265` says existing projects converge only when migrations are authorized. Slim-booty's scope (rewrite or incremental slim) decides whether standards are mandatory now (INFERRED open).
9. **Auth specifics left open in the standard:** IdP choice for the cluster (`WL:799`); `automemlimit` library (`WL:756`); notifier building blocks to evaluate (`WL:529-530`); token contract details, glass style tokens and where the shared token module lives (`WL:830`, open question 6); the token contract is still marked "(draft)" (`WL:217`).
10. **Role-name drift.** The standard writes roles as `color.action.primary`; design-tokens v1 uses the bare path `action.primary` (`DTD:433`). Recorded divergence, not a blocker.
11. **Light-tier risk accepted.** templ-components had about 6 stars and one author; pin in `go.mod`, consider a Forgejo pull-mirror (`WL:186-187`). Relevant if booty goes light.
12. **Not verified by me:** that `task ci` exists in booty (`GS` says it should); whether booty's `/login` and open-route errors use problem+json; what `pkg/db/health.go` reports; whether the generated antd file type-checks on antd v5.

## 8. Quick checklist for a slim-booty feature list (derived, INFERRED selection)

Decided and checkable now (cite `WL`):
- UI embedded in the Go binary via `embed.FS`, no JS runtime at runtime (`WL:168`). booty already does this (`web/embed.go`, `Dockerfile:29-30`).
- Auth: OIDC (PKCE) and/or built-in passkeys; no passwords; machine API keys hashed, scoped, expiring, revocable, `Authorization: Bearer` (`WL:506-514`).
- Admin port 9090 with `/healthz`, `/readyz`, `/health`, `/metrics` as a separate listener (`WL:379`, `WL:696`).
- slog JSON to stdout, snake_case fields, `error` key, wide events (`WL:465-485`).
- Viper with BindStruct, no global viper, `<PREFIX>_DATABASE_URL` (`WL:308`, `WL:673`); bun + goose if the DB layer is touched (`WL:324-325`).
- snake_case JSON, `/api/v1`, problem+json, committed OpenAPI with `oasdiff` (`WL:362-368`).
- `taskfile.yml` with `task ci` (`GS`); distroless `:nonroot`, `USER 65532:65532`, digest-pinned `FROM`, Bun for any JS build (`WL:549-555`).
- Tokens: no raw hex in UI code; consume design-tokens output (antd now, shadcn later) (`WL:219`, `DTD:344-353`).

Needs an owner decision first: React+antd vs light tier (item 7.1); whether booty is appkit's first consumer (7.5); migration authorization (7.8).
