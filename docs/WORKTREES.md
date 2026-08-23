# Worktree hazards, measured (AGENTS.md, "Working in worktrees")

The rules for working in a linked worktree live in
[AGENTS.md](../AGENTS.md#working-in-worktrees) and stay there, because a rule you read only by
following a link is a rule you do not read. This document is what those rules were learned from:
what git was measured doing, the incident that made each one a rule, and the repair procedure for
a tree that has already gone wrong.

Read it when a rule looks like it is in your way, when `make worktree-check` has just failed, or
when a working tree looks like somebody reverted merged work by hand.

## `-B` succeeds where `checkout` refuses

The rule reads as advice about a guard rail git already has — and git does have it, for exactly
one spelling. Measured on git 2.54, in a scratch repo where `shared` is held by a second worktree:

```console
$ git checkout shared
fatal: 'shared' is already checked out at '/tmp/…/wt'

$ git checkout -B shared HEAD
Switched to and reset branch 'shared'
```

The second moves the shared ref out from under the other worktree, exit 0, no warning — and that
worktree's next `git status` reports its whole tree as staged changes, which reads as somebody
having reverted the work by hand. `git switch -C` and `git branch -f` do the same thing; only the
un-forced forms refuse.

So the hazard is not that someone ignores a documented flag. It is that the *safe* form fails loudly
and the *forcing* form succeeds silently, and the forcing form is what anybody reaches for when a
throwaway verification branch already exists. Prefer `git switch --detach <ref>` when the intent is
to look at a commit rather than to hold a branch: it touches no ref, so it cannot take one from
anybody.

## Why the crossing reports no error

Linked worktrees share `refs/heads/*` but hold **separate** indexes and working trees. So
`git checkout main && git reset --hard origin/main` inside a linked worktree advances the shared
`main` ref while writing files into *that* worktree only. The primary checkout's next
`git pull --ff-only` then fast-forwards from the already-advanced ref, writes only the
**remaining** commits' files, and skips the crossed commit's file set entirely — reporting
`Fast-forward` on the way.

What that leaves is worse than a conflict, because it is a *coherent older tree*: it compiles,
`make check` can be green on it, and `git status` reports the skipped commit's files as staged
deletions — which reads as though somebody reverted merged work by hand. Measured on
2026-08-12: `ben-b11` took `main` at 14:15:57 and reset past `b568c4f` (#101), and the primary
tree never received `internal/orchestrator/shutdown.go` or `cmd/ben/daemon.go` at all.

`make worktree-check` detects the cause — the duplicate — and rides `make check` for the reason
this section gives: the tree it leaves behind is one the test suite reports as green.

## Diagnose it by file mtime, not by reflog

The primary's reflog shows only its own operations, and each looks correct. Per-file mtimes
cluster on the git operation that wrote them, so a tree that skipped a commit shows the gap
directly:

| file | mtime | meaning |
|---|---|---|
| `internal/workspace/marker.go` | `08-12 15:06:01` | #107 arrived |
| `cmd/ben/main.go` | `08-12 11:24:41` | pre-#101, never updated |
| `internal/orchestrator/shutdown.go` | absent | #101 never arrived |

`git worktree list` names the duplicate, and each linked worktree's own reflog lives in
`.git/worktrees/<name>/logs/HEAD` — read that one, not the primary's.

**Diagnose before touching anything — every repair below normalizes the tree and destroys the
evidence.** Afterwards the tree equals the commit it was repaired to, so the check that follows
matches that commit trivially and can no longer say what the tree *was*. Run it first, or read it
from the preserved commit.

**Establish what the tree is by proving equality, not by counting paths.** A count cannot separate
*differs because a commit changed it* from *differs because someone edited it*, and
`git diff --cached` answers only for the **index** — blind to the unstaged edits a `reset --hard`
also erases. The claim worth testing is the strong one: tree **and** index identical to a commit
already on `origin/main`, holding nothing of their own.

```sh
for c in $(git rev-list --max-count=20 origin/main); do
  git diff --quiet "$c" && git diff --cached --quiet "$c" \
    && echo "tree is exactly $(git log -1 --format='%h %s' "$c")"
done
```

`git diff <commit>` compares the **working tree** and `--cached` the **index**; both quiet means
neither holds anything that commit does not. A merely-stale tree matches exactly one candidate; one
local edit anywhere and it matches **none**, which is the answer that says stop.

One forensic note: `git stash create` writes the index and leaves a dangling `WIP on <branch>`
commit behind. Do not run it while investigating and then read either as evidence.

## Repair

**Clear the duplicate first — it is the repair, and one route refuses without it.** While another
worktree holds the branch, switching back to it is refused outright, which strands this worktree on
whatever it switched to:

```text
fatal: 'main' is already checked out at '<path>'
```

So get the other worktree off the branch before anything else: `git -C <other> switch --detach`, or
`git worktree remove <other>` when it holds no work of its own.

**Then repair the tree without moving the ref.** `git reset --hard origin/main` does two separate
things at once, and conflating them is what caused this in the first place:

| what is wrong | repair | moves a ref? |
|---|---|---|
| the tree is stale against its own ref | `git reset --hard HEAD` | **no** — safe even while another worktree holds the branch |
| the ref itself is behind | `git pull --ff-only` | **yes** — only once the duplicate is cleared |

Reset to `HEAD`, never to `origin/main`. Where `main` and `origin/main` differ, the second form moves
the **shared** ref while updating only this worktree's tree — the original hazard, from the other
direction. Preserve first, chained, so a failed preservation cannot fall through to the destructive
step:

```sh
git stash -a && git reset --hard HEAD        # -a, not -u: see below
```

**Preserve rather than enumerate: no list of at-risk paths you build yourself is sound.** Measured on
git 2.54 — `reset --hard` destroys at exit 0 with nothing in its output, where a checkout refuses and
changes nothing:

| in the way | `git switch`/`checkout <target>` | `git reset --hard <target>` |
|---|---|---|
| untracked file at a path the target carries | refuses: *"untracked working tree files would be overwritten"* | overwrites it silently |
| **ignored** file at that path | the same refusal | the same silent overwrite — and `stash -u` never parked it: *"No local changes to save"* |
| untracked **file** `d`, target carries `d/f` | refuses, naming `d` | deletes the file, exit 0 |
| untracked `d/f`, target carries a **file** `d` | refuses: *"Updating the following directories would lose untracked files in them"* | deletes the directory, exit 0 |

The last two rows are prefix collisions, not equal paths, so any exact-match comparison of
`git ls-files --others` against the target tree reports **nothing at risk** while the data goes. Two
shapes, two different git messages, and no reason to believe the next shape is enumerable either —
the mistake this repo has already paid for twice (#47, #52, and the writable-set note on #114).

So either `git stash -a`, the only stash that includes ignored files — noting it also sweeps
`.scratch/` and `.claude/` out of the tree — or let git enumerate the exposure by attempting
`git checkout <target>`, which refuses safely and names every file at risk.

## Where a worktree lives

A worktree nested under the gitignored `.claude/` is invisible to git and to gitignore-aware
search, while every tool that walks the filesystem plainly — editors, `find`, indexers — sees a
second full copy of the tree per worktree. That is the whole cost, and it is why the sibling
directory is a hygiene rule rather than a correctness one.

Nothing about BEN's own checks depends on it: `internal/arch` skips dotted directories by name
and nested modules by their `go.mod`, so a worktree in either place is already out of scope.
Neither rule exists to make nesting safe, and removing one would not be paid for by moving
worktrees out — see `isModuleRoot`'s comment in `internal/arch/arch_test.go` for what that rule
is actually for.

## What a detached maintenance run costs

Measured on git 2.39.1, one bare `git fetch` into a repository whose `gc.auto` threshold is
crossed, with `GIT_TRACE2_EVENT` recording what each process started:

| invocation | child forked | `objects/pack` written after the fetch returned |
|---|---|---|
| `git fetch` | `git maintenance run --auto --quiet` | yes — a new pack, within 2s |
| `git -c maintenance.auto=false fetch` | none | no |
| `git -c gc.auto=0 fetch` | `git maintenance run --auto --quiet` | no |

Both rows matter, and they are why BEN passes both keys rather than the one that looks
sufficient. `maintenance.auto=false` is what stops the *fork* — nothing is spawned at all.
`gc.auto=0` is what stops the *work*, which is the leg that answers for a git old enough to have
run `git gc --auto` directly — `run_auto_gc`, before `git maintenance` existed to route it.

They are passed as `-c` (`gitArgv`, `internal/workspace/git.go`) rather than written into
`base.git/config`, because `-c` outranks every config file: the guarantee then holds over a base
repository BEN did not create, cannot be edited out of one an operator configured the other way,
and — measured — reaches the git processes git itself starts, through the `GIT_CONFIG_PARAMETERS`
it exports for them.

The failure that found this was `make check` going red once on a `TempDir` cleanup
(`unlinkat …/base.git/objects/pack: directory not empty`, CI run 32158116390): the test had
finished, and a process it never started was still writing. A daemon pays the same cost
differently — `gc.pid` and pack locks taken inside a workspace an attempt is running in, by a
process BEN cannot account for (SPEC §9.10). `internal/workspace/maintenance_test.go` holds the
invariant at three seams: the argv every provider git is started with, git's own `child_start`
records over a driven lifecycle, and what `git config --get` resolves inside a repository
configured to maintain itself.
