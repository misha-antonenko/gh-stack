# `gs pull` — merge-based stack propagation

## Goal

Add `gh stack pull`, a sibling of `gh stack rebase` that propagates updates
from the base of a stack to the top using `git merge` instead of `git rebase`.
It must offer the same feature surface (range flags, remote handling, trunk
fetch/fast-forward, conflict continue/abort, merged/queued-PR skipping) with
maximal reuse of the existing rebase machinery.

## Rebase vs. merge — the essential difference

`gs rebase` rewrites each branch's history, replaying its commits onto the new
parent. Because rebasing must *drop* commits that a merged PR already landed in
trunk, it carries `--onto`/`oldBase` bookkeeping (`needsOnto`, `ontoOldBase`,
`resolveRebaseOldBase`).

`gs pull` merges each parent into its child, bottom-up. Merging is *additive*:
it never drops commits, so the entire `--onto`/`oldBase` apparatus is
unnecessary. For every active branch the base to merge in is simply its
**effective parent**: the nearest non-merged ancestor, or trunk. A merged
ancestor's squashed change reaches the child through trunk; a queued ancestor's
commits reach the child by merging the queued branch directly.

This makes the pull cascade strictly simpler than the rebase cascade.

## Reuse map

Reused unchanged from `cmd/utils.go` / `cmd/sync.go`:
`loadStack`, `pickRemote`, `resolveTrunkTarget`, `fastForwardBranches`,
`activeBranchNames`, `resolveOriginalRefs`, `updateBaseSHAs`, `syncStackPRs`,
`ensureRerere`, `verifyStacked`, `reportUnstacked`, `restoreRebaseRefs`,
`printConflictDetailsWithContinue`, `cascadeRebaseResult` (shared result type),
`trunkTarget`, `modify.CheckStateGuard`.

New shared helpers extracted (used by both rebase and pull):
- `computeCascadeRange(s, currentIdx, downstack, upstack, noTrunk)` — the
  start/end index math currently inline in `runRebase`.
- `effectiveParentBranch(s, absIdx, trunkRef)` — nearest non-merged ancestor or
  trunk; already duplicated inside `cascadeRebase` and `verifyStacked`.
  `verifyStacked` is refactored onto it; the merge cascade uses it. `cascadeRebase`
  is left untouched to avoid regressing the delicate `--onto` paths.

## New code

### `internal/git` (merge primitives, mirroring the rebase ones)
- `Merge(base) error` → `git merge --no-edit <base>`, with rerere auto-resolve.
- `MergeContinue() error` → `git merge --continue` (GIT_EDITOR=true).
- `MergeAbort() error` → `git merge --abort`.
- `IsMergeInProgress() bool` → `MERGE_HEAD` present in the git dir.
- `MergeStartError` + `IsMergeStartError` — mirror `RebaseStartError`.
- `runMergeCommand` / `tryAutoResolveMerge` — mirror the rebase helpers. A merge
  is a single commit, so auto-resolve needs no replay loop.
- Wired through `Ops`, `defaultOps`, `MockOps`.

### `cmd/utils.go`
- `cascadeMerge(opts cascadeRebaseOpts) cascadeRebaseResult` — for each branch in
  range: skip merged/queued (print), else `git checkout` + `git merge
  effectiveParent`. On conflict return the conflict + remaining names; on a
  start failure return `Err`. No `--onto` state.

### `cmd/pull.go`
- `pullOptions` — branch, downstack, upstack, cont, abort, noTrunk, remote.
  (No `--committer-date-is-author-date`; that is rebase-only.)
- `pullState` — CurrentBranchIndex, ConflictBranch, RemainingBranches,
  OriginalBranch, OriginalRefs, NoTrunk, TrunkRef, TrunkSHA, StartIndex,
  EndIndex. (No onto/committer-date fields.)
- `pullStateFile = "gh-stack-pull-state"`.
- `PullCmd`, `runPull`, `continuePull`, `abortPull` — mirror the rebase trio,
  reusing the helpers above. Conflict recovery aborts an in-progress merge
  before restoring refs.

### Wiring & docs
- Register `PullCmd` in `cmd/root.go`.
- README: add a `gh stack pull` section paralleling `gh stack rebase`.

## Tests
- `internal/git`: `runMergeCommand` start-vs-conflict classification.
- `cmd/pull_test.go`: cascade order (merge parent into each child), merged-PR
  skip, queued-PR skip, `--downstack`/`--upstack`/`--no-trunk` ranges, conflict
  → state saved → `--continue`, `--abort` restores refs, "no branches" cases.
- Existing rebase/sync tests must stay green (they cover the extracted helpers).

## Status
- [x] Plan written
- [x] git merge primitives + mock + tests
- [x] extracted helpers (`computeCascadeRange`, `effectiveParentBranch`)
- [x] `cascadeMerge`
- [x] `cmd/pull.go` + registration
- [x] pull tests
- [x] README section
- [x] full suite green
