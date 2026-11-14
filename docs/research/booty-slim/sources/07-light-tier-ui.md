# Light-tier UI: templating and component set

Scope: the two choices D3 leaves open for the slim booty UI (README `D3`, line 273): `html/template` vs templ, and
which shadcn-style component set. Method: read-only. Shallow clones in `/tmp/lighttier-research/`, `gh api` for repo
metadata (fetched 2026-11-12), jsDelivr/npm registry for packages. Nothing was built or rendered.

Pins used for every citation below (`repo@sha:path:line`):

| Repo | Commit (date) | Latest release |
|---|---|---|
| `a-h/templ` | `9a0eb3c` (2026-11-12) | v0.3.1118 (2026-11-12) |
| `axadrn/shadcn-templ` (= `templui/templui`, `axzilla/templui`, both redirect) | `3297df4` (2026-11-11) | v2.0.0 (2026-10-30) |
| `LarsArtmann/templ-components` | `a866677` (2026-11-12) | v1.27.0 (2026-11-04) |
| `hunvreus/basecoat` | `6769c83` (2026-10-23) | 1.1.0 (2026-10-23) |
| `productdevbook/shadcn-htmx` | `7c96261` (2026-06-03) | tag v0.0.2, no releases |
| `bigskysoftware/htmx` | `8d112c7` = v4.0.1 (2026-11-01) | v4.0.1; v2.0.11 same day |

Owner docs: WL = `~/Projects/local/spike/openspec/docs/workflow-lenses.md`; GFD = github-feed design
`docs/designs/2026-11-05-github-feed-design.md`; GFV = github-feed `internal/web/VENDORED.md`; DTD = design-tokens
`docs/designs/2026-11-10-design-tokens-v1-design.md`; GS = `~/.config/reference/go-standards.md`.

## 1. What the owner's standard already decides

The standard decided both questions on 2026-11-09. Quoted lines:

- WL:160: "Status: **decided 2026-11-09. Documented only — no app migration or port is authorized.**"
- WL:168: "Every UI is embedded in its Go binary (`embed.FS`). No JavaScript server runs at runtime."
- WL:172: "| Stack | Go + **templ** + **htmx** + Tailwind (standalone CLI) | ..."
- WL:173: "| Primary components | **shadcn-templ** (`axadrn/shadcn-templ`, formerly templUI) | **antd** |"
- WL:174: "| Secondary components | **templ-components** (`LarsArtmann/templ-components`) | ..."
- WL:178: "| Build toolchain | Go tooling only, no Node | ..."; WL:180: "| Token output | shadcn CSS variable names | ..."
- WL:182-184: "Why templ over `html/template`: `{{template}}` takes one argument and has no children, so components need `dict`
  helpers, `template.HTML`, and `HTMLAttr` — the constructs the web security rules forbid. templ components are typed Go
  functions with `{ children... }` and `templ.Attributes`, checked at compile time; templ installs as a Go tool."
- WL:186-187: "Known risk: templ-components had ~6 stars and one author on 2026-11-09. Accepted by the owner; pin it in
  `go.mod` and consider a Forgejo pull-mirror."
- Rejected (WL:193-195): "`html/template` | Composition forces the constructs the security rules forbid"; "Basecoat |
  Bus factor 1 (88% of commits, sole npm maintainer); templ-native kits fit better"; "shadcn-htmx | Written for
  `html/template`".
- WL:211: "| booty | React + antd | Light | Forms and plain tables only |"
- Token contract WL:217-249: "**Pick a role, never a value.**" (WL:219); feedback roles "`warning`, `danger`, `success`,
  `info`. shadcn has only `destructive`, so the contract adds the rest as extra variables" (WL:230-231); "Tests: axe
  accessibility checks under Playwright in every declared mode" (WL:254).
- DTD:32: "The shadcn CSS-variable output (light tier): one more generator, added when a light-tier consumer is
  authorized." So the shadcn output does not exist yet; booty would be its first consumer.

Discrepancy to fix: booty-slim `README.md:273` says "Still open inside the light tier: `html/template` vs templ,
shadcn-htmx vs Basecoat (WL:173-174)". WL:173-174 name shadcn-templ and templ-components, and WL:193-195 reject both
shadcn-htmx and Basecoat. The README cites the right lines with the wrong names.

"The web security rules" (WL:183) are not a file under `~/.claude/rules/` or `~/.config/reference/`. Their written
form is GFD §7.9 (GFD:582-612) plus github-feed's `bannedUI()` list (`internal/web/ui_parse_test.go:22-29`):
`dict`, `deref`, `htmlSafe`, `.Attrs`, dynamic element/attribute names, `style=`, `<script`, `hx-on`, `javascript:`.

GS mentions neither templ nor `html/template` (grep for "templ" finds only "template" in unrelated lines 419, 575,
1645). Its rules that bite: "Pin the `go` directive to the lowest `1.N.0` your dependencies allow" (GS:104); Huma is
the HTTP framework for services (GS:35), which github-feed set aside because "this is an HTML app, not an API service"
(GFD:80). booty keeps an OpenAPI API (README:273), so Huma for the API plus plain handlers for HTML is the INFERRED fit.

## 2. templ vs `html/template`

| Criterion | `html/template` (Go 1.27.3 stdlib) | templ v0.3.1118 |
|---|---|---|
| Type safety | Data is `any`; a wrong field fails at `Execute`, not compile | Components are Go funcs with typed params; `go build` fails on a wrong field (WL:184; `docs/docs/04-core-concepts/01-components.md`) |
| Composition | `{{template "name" pipeline}}`: one pipeline, no children (`go doc text/template`, "{{template "name" pipeline}}") | `{ children... }` (`docs/docs/03-syntax-and-usage/10-template-composition.md:88`), `templ.Attributes` spread (`03-attributes.md:198`) |
| Contextual escaping | Yes: HTML, CSS, JS, URI contexts, escapers inserted at parse time (`go doc html/template`, "Contexts") | Text escaped; URL attrs through `templ.URL`; `on*` and `hx-on*` take only `templ.ComponentScript`; no variables inside `<script>`/`<style>`; spread attrs sanitized at render time (`docs/docs/10-security/01-injection-attacks.md:5,20,63-81`) |
| Escape hatches | `template.HTML`, `HTMLAttr`, `JS`, `CSS`, `URL` types bypass escaping | `templ.Raw`, `templ.SafeURL`, `templ.SafeClass`, `templ.JSUnsafeFuncCall` (`17-rendering-raw-html.md:3`; `01-injection-attacks.md:42,69,79`) |
| Known gaps | None found in docs | Docs: "templ only handles htmx event handler attributes"; `hx-vals`/`hx-headers` `js:` values, `hx-vars`, `hx-trigger` filters are not protected (`01-injection-attacks.md:84`). Advisories GHSA-94c2-xg24-pwhv (medium) and GHSA-jq3r-rjqp-mwgj (low), both published 2026-10-26, fixed in v0.3.1100 |
| CSP nonces | Manual: pass the nonce as data | `templ.WithNonce(ctx, nonce)`; `templ.GetNonce(ctx)` (`10-security/02-content-security-policy.md:19,55`) |
| Build step | None; `//go:embed` + `ParseFS` (github-feed does this, `ui_parse_test.go:52`) | `templ generate` → `*_templ.go`; nothing to embed for markup, only CSS/JS assets |
| Committed output | n/a | Docs: "It's common practice to commit generated `*_templ.go` files"; CI runs `templ generate` then `git diff --exit-code` (`09-developer-tools/06-cicd.md:3-12`) |
| Install | stdlib | `go get -tool github.com/a-h/templ/cmd/templ@latest`, run as `go tool templ` (`02-quick-start/01-installation.md:18,24`) |
| LSP/editor | gopls only, no template awareness | `templ lsp` via gopls; VS Code, Neovim, JetBrains, Vim, Helix, Emacs documented (`09-developer-tools/02-ide-support.md:3-428`). Owner's editor Gram (`app.liten.Gram` 3.4.1) is not listed; Zed's registry has `templ` 0.0.11 (`zed-industries/extensions` `extensions.toml:5629`). Whether Gram loads Zed extensions: UNVERIFIED |
| htmx fragments | Name a sub-template and `ExecuteTemplate` it | `@templ.Fragment("name")` + `templ.WithFragments` (`03-syntax-and-usage/19-fragments.md`); or call the row component directly |
| SSE | Render to a buffer, write `data:` lines | Same: `Render(ctx, w)` into a buffer. No SSE helper (none found in docs) |
| Performance | — | Only the author's benchmark: 369 ns/op vs 2,475 ns/op for `html/template`, 2023-08-17 (`benchmarks/templ/README.md`). Not independent; irrelevant at ~12 hosts |
| License | BSD-3 (Go) | MIT |
| Cadence | Go release cycle | 8 releases 2025-08-15 → 2026-11-12; gaps up to 5 months (v0.3.1020 2026-05-10 → v0.3.1070 2026-10-04) |
| Maintainers | Go team | 175 contributors, `a-h` 746/1,125 commits (66%), `joerdav` 69; last 6 months 43 of ~80 commits by a-h |
| Stability | Go 1 compatibility promise | Still v0.3.x; no v1 or compatibility policy found in README or docs at `9a0eb3c` |
| Fit with GS | Zero deps | Raises the `go` directive floor to 1.26.0 (templ `go.mod:3`) and adds its tool deps (fsnotify, testify, x/tools...) to `go.mod` via the tool directive |

Findings:

1. **The standard's composition argument holds, with evidence.** github-feed had to split `button` and `card` into
   `_open`/`_close` pairs because a called template cannot take a body (GFV, button and card rows; `card.tmpl` usage
   comment). templ's `{ children... }` removes that.
2. **templ's escaping is stronger than `html/template` where htmx lives**, since it types `hx-on*`. It is weaker than
   its own reputation for other htmx attributes (docs line 84), so GFD:612's rule "never interpolate forge-supplied
   text into any `hx-*` attribute" still has to stand as a lint rule.
3. **The escape hatches move, they do not vanish.** `templ.Raw` is the new `template.HTML`. github-feed's
   unexported-constructor `SanitizedHTML` pattern (GFD:585) ports directly; a lint bans `templ.Raw`, `templ.SafeURL`,
   `templ.JSUnsafeFuncCall` outside one package.
4. **Two advisories in October 2026** show the escaping layer is young code. Pin ≥ v0.3.1100 (shadcn-templ and
   templ-components already require v0.3.1100 in their `go.mod`).
5. **Generated code: commit it.** It keeps `go install` and `go build` working without the CLI, matches the docs, and
   fits GS's golden-file posture (regenerate explicitly, check in CI).

## 3. Component sets

| | shadcn-templ | templ-components | Basecoat | shadcn-htmx |
|---|---|---|---|---|
| License | MIT | MIT | MIT | MIT |
| Stars / forks / open issues | 1,799 / 104 / 5 | 7 / 0 / 3 | 4,439 / 149 / 10 | 77 / 7 / 2 |
| Commits, top author share | 2,208; `axadrn` 2,084 (**94%**), 28 contributors | 3,151; Lars Artmann 3,148 (**100%**), AUTHORS lists one | 286; `hunvreus` 251 (**88%**), 17; sole npm maintainer | 54; one author (100%) |
| Last commit | 2026-11-11 | 2026-11-12 (72 commits on 2026-11-07 alone) | 2026-10-23 | **2026-06-03** (idle 5 months) |
| Native to | templ | templ | Framework-agnostic HTML/CSS (+ Jinja/Nunjucks macros, `src/templates/`) | `html/template` + 4 other flavours |
| Consumed as | Copy-in CLI `shadcn-templ add` (go tool); fetches from live `https://shadcn-templ.com` (`cmd/shadcn-templ/registry/api.go:18`), `--registry <url>` overrides (`main.go:56`) | Go module, `go get` (README:66-73) | npm `basecoat-css` or CDN; dist files downloadable as plain CSS/JS | Copy-in, `npx shadcn-htmx` CLI (README:55) or by hand |
| Node at build | No: standalone Tailwind v4.3.3 + Go esbuild (`templates/templ-app/Dockerfile`, `Taskfile.yml`) | No (README:13) | Only for the npm path; vendoring `dist/` needs none | CLI needs Node; hand copy does not |
| Tailwind v4 | Yes (`assets/css/globals.css:216`) | Yes | Yes (`src/css/base/base.css:1`) | Yes |
| JS | Vanilla Base UI ports, one bundled file, `MutationObserver` lifecycle (`components/baseui/lifecycle.js`) | Vanilla, "HATEOAS" (README:131) | Vanilla, 11 components need JS, `MutationObserver` (`src/js/basecoat.js:113`) | One shared keyboard layer (README:29) |
| htmx | htmx-agnostic; parity suite runs htmx 4 (`parity/htmx/`); own site loads 4.0.0-beta6 | **htmx 2.0.10** (`CONTEXT.md:17`); uses v2 event names `htmx:afterRequest`, `htmx:afterSwap`, renamed in htmx 4 (htmx `whats-new-in-htmx-4.md:164-166`) | Agnostic; history restore needs `initAll({force:true})` (`installation.mdx:284`) | htmx 4 (README:7) |
| shadcn variable names | Yes, incl. `--sidebar`, `--chart-*`; `:root` + `.dark` (`globals.css:274,325`) | **No**: own `--color-tc-*` (`templ-components-theme.css:96-131`) | Yes, incl. `--sidebar`, `--chart-*` (`base.css:5-63`) | Yes, but no `--sidebar`/`--chart-*` (`app/styles/input.css:46-84`) |
| Dark mode | `.dark` class variant | `.dark` or OS (`theme.css:32-51`) | `html.dark` | `.dark` |
| Dialog/sheet a11y | Ported Base UI focus manager, scroll lock, `inert` backdrop (`components/dialog/dialog.js:153-236`) | Native `<dialog>` (`display/modal.templ:5-11`) | Native `<dialog>`+`showModal()` (`dialog.mdx:62`) | Native `<dialog>` (`dialog.tmpl:38`) |
| A11y tests | Playwright `parity/a11y.mjs` vs shadcn reference; no axe | axe present (56 files) | None found | axe-core + APG keyboard suite (README:35,106) |
| CSP (`script-src 'self'`, `style-src 'self'`) | External bundle with nonce (`components/scripts.templ:5-7`); 0 inline handlers; **17 templates use `style=`** incl. progress width (`progress.templ:154`), dialog and sheet (`dialog.templ:176`, `sheet.templ:159`) | Nonce via `BaseProps` (README:58) | **Inline `onclick` in docs markup**: 29 in dialog, 22 alert-dialog, 25 drawer; 0 in dropdown/tabs/select | 9 templates with `<script>`, 4 with inline handlers |
| Stability | v1→v2 major on 2026-10-30, module `/v2` | v1.16 → v1.27 in 8 weeks | 1.0.0 2026-06-28, semver since | v0.0.2 |

MDN (`mdn/content@aa3f3f5`, `.../content-security-policy/style-src/index.md:88-98`): "Inline style attributes are also
blocked" under a `style-src` without `'unsafe-inline'`, as are `setAttribute("style", …)` and `cssText`. A nonce
covers `<style>` elements, not attributes (same file, line 133).

### Coverage matrix

✓ = component exists at the pin; ~ = buildable from parts the kit has; ✗ = absent.

| Need | shadcn-templ | templ-components | Basecoat | shadcn-htmx |
|---|---|---|---|---|
| table | ✓ `table` | ✓ `display/table` | ✓ CSS | ✓ `table`, `selectable-table` |
| badge | ✓ | ✓ `badge`, `count_badge`, `status_dot` | ✓ | ✓ |
| tabs | ✓ | ✓ | ✓ JS | ✓ |
| sheet / drawer | ✓ `sheet`, `drawer` | ✓ `drawer` | ✓ `drawer` | ✓ `sheet` |
| dropdown menu (row kebab) | ✓ `dropdownmenu` | ✓ `dropdown` | ✓ JS | ✓ |
| dialog + typed-confirm input | ✓ `dialog`, `alertdialog` + `input`, `field` | ✓ `modal` + `forms/input` | ✓ `dialog`, `alert-dialog` | ✓ `dialog`, `alert-dialog` |
| toast (server-pushed) | ✓, server-side toast via swap (`toast.templ:133`) | ✓ `feedback/toast` | ✓ "from backend (with HTMX)" (`toast.mdx:14`) | ✓ |
| select | ✓ `select`, `nativeselect` | ✓ | ✓ JS | ✓ |
| input | ✓ | ✓ | ✓ | ✓ |
| collapsible / tree | ✓ collapsible; ~ tree via `blocks/sidebar11` (file tree from Collapsible) | ✓ `collapsible_section`; ✗ tree | ✓ `details` CSS; ✗ tree | ✓ `collapsible`, **✓ `tree`, `treegrid`** |
| card (KPI tiles) | ✓ `card` | ✓ `card`, `StatCard` | ✓ | ✓ |
| sidebar nav | ✓ + 16 sidebar blocks | ✓ `sidebar_nav`, `appshell` | ✓ JS | ✓ |
| spinner / progress | ✓ `spinner`, `progress` | ✓ `loading`, `progressbar`, `circular_progress` | ✓ `spinner`, `progress` | ~ `progress`, `meter`, `skeleton`; no spinner |
| tooltip | ✓ | ✓ | ✓ | ✓ |

Every kit covers the list except the tree. shadcn-htmx is the only one with an ARIA tree; shadcn-templ gets there
with the sidebar11 pattern.

## 4. github-feed lessons (one data point)

github-feed vendored shadcn-htmx `7c96261` with `html/template`, htmx 4.0.1 and the standalone Tailwind v4 CLI
(GFD:77-79, GFV). Six components were rewritten, not copied. What broke and why:

| What | Upstream construct | Why it broke | Fix in github-feed |
|---|---|---|---|
| badge, button, avatar sizes/variants | `dict`/`index` lookups | sprig-style helpers banned | `if`/`else if` chains |
| badge `Tag`, card `TitleAs` | dynamic element names | `<{{` banned | fixed `<span>` / `<div>` |
| card `Content`/`Footer`/`Action` | `template.HTML` params | no children in `{{template}}`; `template.HTML` banned | `card_open`/`card_close` pair |
| button label, `Attrs` | `template.HTML`, `HTMLAttr` map | same | `button_open`/`_close`; explicit `PostURL`/`GetURL` |
| avatar `onerror` | inline handler | `script-src 'self'` | `data-fallback` + static JS |
| input `Spellcheck` | `deref` | helper banned | removed |
| Google Fonts in `input.css` | remote stylesheet | `style-src 'self'` | system font fallback |

Sources: GFV table rows; ban list `ui_parse_test.go:22-29`; CSP at GFD:607-608. Scale: 67 of shadcn-htmx's 82 Go
templates use `dict`, `default` or `deref` (grep at `7c96261`), so the rewrite cost grows with every component taken.

Lessons that carry over to templ: (a) two of the seven rows (children, dynamic attrs) are what templ fixes; (b) the
other rows are CSP and taste rules that bite any kit, including shadcn-templ's 17 `style=` templates and Basecoat's
inline `onclick`s; (c) a guard test that scans vendored markup for banned constructs worked and should be kept.

## 5. Recommendations

### Q1, templating: **templ** (agrees with WL:172, WL:182-184)

For: typed components and `{ children... }` remove the open/close splitting and the sprig rewrites that cost
github-feed six components; `hx-on*` is typed; `templ.WithNonce` covers the nonce plumbing; two of the three
candidate kits are templ-native. Install as a Go tool (WL:184), commit `*_templ.go`, and add `go tool templ generate
&& git diff --exit-code` to `task ci`.

Against (the honest case for `html/template`): zero dependencies and the Go 1 promise, versus a v0.x tool with one
66% author and two advisories last month; no codegen step or generated-file churn; github-feed already proves it under
the strict CSP; the GS `go` directive rule is untouched. If booty used only five plain components, `html/template` with
hand-written partials would be adequate. It loses because booty needs dialogs, sheets, menus and a tree, which is
where the composition limits show (section 4).

### Q2, component set: **shadcn-templ, copied in at `3297df4` (v2.0.0)**, no secondary kit for booty

For: templ-native; shadcn CSS-variable names including `--sidebar` and `--chart-*`, so the planned DTD output maps 1:1;
covers 13 of 14 needs and the tree via the sidebar11 pattern; no Node; htmx 4 swaps are tested in its parity suite;
real focus management in dialog/sheet; active (v2.0.0 twelve days ago).

Where this differs from the standard:

1. **Drop templ-components as booty's secondary (differs from WL:174).** At `a866677` it targets htmx 2.0.10 and
   htmx-2 event names, which htmx 4 renamed; it uses its own `--color-tc-*` tokens, so the token contract would need a
   second mapping; it raises the `go` floor to 1.27 (`go.mod:3`); one author, 100% of commits. booty's needs are met
   without it. Case for keeping it: typed enums, `StatCard`, axe tests, and it is a normal Go module, easy to pin.
2. **The bus-factor reason used against Basecoat (WL:194) applies more strongly to shadcn-templ** (94% vs 88%). The
   standard should say why it is accepted: copy-in means booty owns the code after `add`, so the upstream's health
   matters only for future pulls. Record that in WL so the reasoning is consistent.
3. **Do not use the CLI's default registry.** It pulls from a live website, not a commit (`registry/api.go:18`).
   Copy files from the pinned checkout, or serve that checkout's registry locally and pass `--registry`.

Case for Basecoat instead: biggest community (4.4k stars), semver 1.x, native `<dialog>`, shadcn variable names, plain
CSS that any templating can wrap, so the kit does not tie booty to templ. Against: no templ or Go templates, so every
component is hand-written markup; its dialog/drawer examples rely on inline handlers that `script-src 'self'` blocks;
no tree. It is the best **fallback** if shadcn-templ goes idle: the variable names match, so a theme carries over.

Case for shadcn-htmx: htmx-4-native, the only ARIA `tree`/`treegrid`, an axe + APG test suite. Against: `html/template`
with sprig in 67 of 82 templates, idle since 2026-06-03, v0.0.2, one author, missing `--sidebar`. Use its `tree.tmpl`
as an ARIA **reference** when writing booty's inventory tree in templ, not as a dependency.

## 6. Risks and mitigations

| Risk | Mitigation |
|---|---|
| shadcn-templ goes idle or rewrites again (v1→v2 in Oct 2026) | Copy-in at a pinned commit; Forgejo pull-mirror of `axadrn/shadcn-templ`; record the commit per file in a `VENDORED.md` like GFV; Basecoat as the fallback skin |
| CLI registry drift (live site) | Never run `add` against the default; copy from the pinned checkout or `--registry` at a locally served copy |
| `style=` in progress, dialog, sheet, drawer vs `style-src 'self'` | Rewrite on copy-in: progress width via `<progress>`/`data-*` + CSS or per-property CSSOM writes from the bundle (MDN lists only `setAttribute`/`cssText` as blocked; per-property writes UNVERIFIED); keep github-feed's `style=` ban test |
| Bundle step pulls heavy tool deps (`go tool shadcn-templ bundle` brings esbuild, gg, goldmark: `go.mod:5-16`) | Serve the copied per-component `.js` files as static assets instead (INFERRED to work; check during build), or vendor the built bundle with a checksum |
| templ v0.x churn or escaping bugs | Pin ≥ v0.3.1100 in the tool directive; Renovate on it; CI regeneration diff; lint bans `templ.Raw`/`SafeURL`/`JSUnsafeFuncCall` outside one package |
| `hx-*` attributes templ does not sanitize | Keep GFD:612's rule as a lint: no untrusted data in `hx-vals`, `hx-headers`, `hx-trigger`; leave htmx's `inlineScriptNonce` unset; consider htmx 4's `hx-csp` extension (`hx-nonce` gating) |
| SSE OOB swaps under htmx 4 | `hx-sse` is an extension in 4.0.1 (`src/ext/hx-sse.js`); `hx-swap-oob` and `<hx-partial>` work inside SSE messages (`extensions/02-hx-sse.md:139-146`); serve the extension from `'self'` |
| Token output not built (DTD:32) | Booty starts on shadcn-templ's default `globals.css` variables; swap to the DTD generator's output when it lands; add the `warning`/`success`/`info` extras the contract requires (WL:230-231) |
| No axe in shadcn-templ | WL:254 already requires axe under Playwright in each mode; run it on booty's pages, not the kit's |
| Gram editor has no documented templ support | Check whether Gram installs Zed's `templ` extension (UNVERIFIED); otherwise rely on `go tool templ fmt` and gopls |
