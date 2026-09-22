# doublewordai/snapshot fork layout

Integration fork of ai-dynamo/snapshot. The fleet's snapshot operator and
node agent images are built from a commit of this repo's `main`.

## Branches

- `upstream-base`: exactly the upstream release tag the fork is based on.
  Currently `v0.1.0`. Never carries our commits.
- `main`: `upstream-base` plus every patch branch below, merged with
  `--no-ff` in stack order. `git log --merges upstream-base..main` is the
  patch list.
- `upstream-pr/<topic>`: a change we intend to land upstream. Based on
  `upstream-base`. This repo is a public fork, so the branch heads the
  upstream PR directly once rebased onto upstream `main`.
- `vendor/<topic>`: a Doubleword-only change that will not go upstream.
  Based on `upstream-base`.
- `archive/*` and other branches are history and are not part of any image.

## Rules

- No backports. Do not cherry-pick upstream commits onto `main`; a fix that
  is in a newer release arrives by moving `upstream-base`.
- One branch per patch, atomic, with the reason in the commit message.
- Moving to a new release: point `upstream-base` at the tag, rebase each
  patch branch that is still needed onto it, drop the ones the release
  contains, rebuild `main` as base plus merges, force-push `main`, build
  images. Update the stack list below.

## Current stack

- `vendor/fork-layout`: this section.

## Planned stack (agreed 2026-09-22)

Re-derived from the patches the dynamo fork carried against its in-tree
snapshot before upstream moved snapshot to this project:

- `upstream-pr/criu-memory-compression`: CRIU memory compression option.
- `upstream-pr/reject-partial-artifacts`: reject partial artifacts and
  bound bakes.
- `upstream-pr/restore-pod-target-label`: select restore pods by target
  label.
- `vendor/runtime-storage-path`: override the runtime storage path
  (MicroK8s containerd layout).

