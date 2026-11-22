# Project Status

This page is a status report: the milestones that are done, the work that is
left, the open follow-ups and the contributor conventions.

## Done

Each row names the design and the plan of one milestone. They are in planning
directories, so this page shows their paths and does not link them.

| Milestone | Design | Plan |
|---|---|---|
| M1: boot plane | `docs/designs/2026-11-13-hephaestus-core-design.md` | `docs/plans/2026-11-14-hephaestus-m1-core-implementation.md` |
| End-to-end harness (part of M1) | `docs/designs/2026-11-13-hephaestus-core-design.md` | `docs/plans/2026-11-16-e2e-harness-implementation.md` |
| M2: utility boots | `docs/designs/2026-11-16-hephaestus-m2-utility-boot-design.md` | `docs/plans/2026-11-16-hephaestus-m2-utility-implementation.md` |
| M2: parallel fetch | `docs/designs/2026-11-17-hephaestus-parallel-fetch-design.md` | `docs/plans/2026-11-17-hephaestus-parallel-fetch-implementation.md` |
| M3: First Talos Install | `docs/designs/2026-11-18-hephaestus-m3-first-install-design.md` | `docs/plans/2026-11-19-hephaestus-m3-first-install-implementation.md` |

For M3, see also [Install Talos](first-install.md).

## Left

In this order:

1. **M3b: Offline Installs and Preseed.** No design exists yet. The scope is in
   the Milestones section of `docs/designs/2026-11-13-hephaestus-core-design.md`.
2. **M4: auth, UI and operations.** This is the last milestone of v1.
3. **Alpine aarch64.** It waits for a spike (D18).
4. **Image publishing.** It has no milestone. See section 3 of
   `docs/designs/2026-11-13-hephaestus-core-design.md`.

## Open Follow-Ups

- One security-audit finding waits for an owner decision. The details are kept
  outside this repository.
- `cluster list` does not show the file and the rule of a failed tree. The
  display needs a schema change.
- Three minor items from M3 are deferred.
- The diagram in [How It Works](../README.md#how-it-works) shows only M1 and M2.

## Contributor Conventions

- Sign every commit.
- The author is `jacaudi`, with the GitHub noreply address, set in the
  repo-local git config.
- `docs/prompts/` stays untracked.
- The integration path for new work (a local merge or a PR) is not decided yet.

[← Back to README](../README.md)
