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
- `upstream-pr/runtime-storage-path`: `runtime.storagePath` chart value for
  the agent's runtime storage mount (MicroK8s keeps containerd state under
  `/var/snap/microk8s/common`). Fork PR #2.

`upstream-base` stays on the 0.1 release line while the Dynamo operator
pins `github.com/ai-dynamo/snapshot/api v0.1.0`; move it together with that
pin.

## Not carried (re-checked 2026-09-23 against v0.1.0)

The Dynamo fork's in-tree snapshot carried four more patches:

- Partial artifacts: v0.1.0 stages every capture under `.tmp` and renames
  it into place only after all phases succeed.
- Restore pod selection: v0.1.0 restores only pods with the explicit
  `nvidia.com/restore-from` annotation, which cleanup pods never carry.
- Bake limits: a capture is a SnapshotJob-owned batch Job. Its priority
  class comes from the DGD component's `checkpoint.job.podTemplate`, and a
  namespace ResourceQuota scoped to that class bounds concurrent bakes.
- CRIU memory compression: needs an LZ4-enabled CRIU build; not ported
  while checkpointing is disabled in the fleet and upstream is moving
  checkpoint I/O to PageBroker.
