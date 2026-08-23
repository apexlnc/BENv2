# BEN — Agent Guide

BEN is a single-binary Go daemon that works GitHub Issues autonomously with coding agents
(Claude Code, Codex) in isolated git worktrees, verifying results from git facts. This file
is the entry point for any agent (or human) working on BEN's own code.

## Source of truth

- **[SPEC.md](SPEC.md)** — the **locked** v1 specification: design invariants (§3), the five
  component contracts, state machine, testing strategy. Implement what it says; do not
  re-litigate its decisions or edit it without explicit human sign-off.
- **[BUILD.md](BUILD.md)** — the build plan. A ticket (B01–B12) is the unit of work; its
  acceptance criteria are the definition of done. The "Assembly-level decisions" section at
  the bottom is settled — don't reopen it.
- `.scratch/` is a gitignored local planning record; never reference it from committed code.

## Repo map

| Path | What it is |
|---|---|
| `cmd/ben` | CLI entry point and assembly for `ben config effective`, `ben run`, and `ben status`. The place components that may not import each other are bound together — including the §9.7 checker and the loop that routes it (`verifier.go`) (SPEC §11) |
| `internal/core` | Shared interfaces and closed enums (SPEC §6–8). Stdlib-only — imports nothing else |
| `internal/config` | Strict `WORKFLOW.md` loader and hot-reload watcher (SPEC §5) |
| `internal/template` | Strict Liquid template layer (SPEC §5.6) |
| `internal/registry` | The one kind table: `tracker.kind`/`agent.kind`/`credential_sources[].kind` → registered kind, asked by both the loader and `ben config effective` (SPEC §5.7, §11) |
| `internal/credential` | The credential source kinds behind `credential_sources` (SPEC §5.2.10, §10.2): `octo_sts`, and `static` for compatibility and development. Each kind's `Describe` is **pure** — no network, no filesystem, no instance — because it is what load-time validation and §5.4's reload comparison call, which is also what keeps `make workflow-check` credential-free. `credtest` is the conformance suite both kinds run unmodified, so a future `github_app` inherits the proof |
| `internal/tracker/github` | GitHub tracker adapter: read kernel + closed write set (SPEC §8) |
| `internal/workspace` | Git worktree lifecycle, safety invariants, hooks, the durable claim-epoch/base store, and publish-evidence git facts (SPEC §6, §9.7) |
| `internal/agent/harness` | Shared process-per-attempt runtime behind both agent adapters: lifecycle, liveness, signal ladder, child-environment composition, transcripts (SPEC §7.2–§7.6). `lifecycle.go` is the pure decision layer — which terminal event, and when it may be published; `handle.go` is the I/O driver around it; `timings.go` is the one seam for its windows |
| `internal/agent/claudecode` | claude-code runner adapter: provider block, argv, stream translation, readiness probes (SPEC §7.7) |
| `internal/agent/codexexec` | codex-exec runner adapter: the same, for `codex exec --json` (SPEC §7.7) |
| `internal/agent/agenttest` | The AgentRunner conformance suite both adapters run unmodified (SPEC §7.1–§7.6) |
| `internal/verify` | Publish-evidence check: the three legs of git and tracker fact behind `done` (SPEC §9.7) |
| `internal/orchestrator` | The authority loop: states, tick, dispatch, retry, reconciliation (SPEC §9) |
| `internal/fake` | In-memory tracker/workspace/runner + manual clock, shared by the orchestrator's tests, `cmd/ben`'s end-to-end acceptance tests, and B12. Not test files — several packages need them, so a fake's fidelity to the adapter it stands in for is a correctness concern (see Conventions) |
| `internal/state` | The §10.3 white-box state dir: run records, the persisted §9.11 transition log, the per-attempt outcome log behind `ben status`'s aggregate (#60), and where transcripts go. Read by a *different process* than the one writing it, which is why every file here is replaced by rename or appended one whole record at a time — `jsonl.go` is the single append-only writer both logs use, and `internal/orchestrator/durable.go` the single off-loop queue that feeds them |
| `internal/integration` | B12's §12.3 invariant suite: SPEC §3's design invariants as end-to-end scenarios. The loop, loader, watcher, template layer and §9.7 checker are real; only the world outside the process is faked, so the suite is green in CI with no network, no subprocesses and no wall-clock waits. Its `doc.go` carries the §12.3 coverage map — including the rows deliberately asserted at another package's boundary |
| `internal/bench`, `cmd/benchreport` | The #62 fixed cohort, session join, matched-case arithmetic, and its documented query (`docs/BENCH.md`). Reads files only; the separate command is the sole importer, so the daemon cannot reach benchmark telemetry |
| `internal/arch` | Structural test enforcing the import boundaries below |
| `internal/partest` | What keeps `make check`'s test time down without weakening it (#167): a gate bounding how many of a package's tests run at once, and the source audit deciding which may join one. The bound is the design — these suites drive real child processes, and unbounded `t.Parallel` would trade elapsed time for the load flakes they exist to expose. Used by `internal/agent/agenttest`, both adapters, and `internal/workspace` |
| `deploy/ben.service` | Sample systemd unit. `KillMode=mixed`, `TimeoutStopSec` and the §10.1 mode statement are load-bearing — `cmd/ben`'s test holds the file to what the daemon claims about it |
| `scripts` | The §12.4 real-integration smoke runner and exact workflow, plus #62's load-validated adapter/model benchmark profiles |
| `docs` | Long-form references kept out of the root, because this guide is loaded into every agent's context and they are not. For an operator: `DEPLOY.md`, the §10.1/§10.2 unattended-operation runbook — the account, the credentials, the branch protection the review gate rests on; and `SMOKE.md`, the §12.4 smoke profile — one issue end to end against a canary repo, the one check that needs credentials and network; and `BENCH.md`, the #62 adapter benchmark — the cohort, the per-cell procedure, and how to read the comparison. For a contributor, the evidence behind three of this guide's rule sections, each linked from the section it belongs to: `WORKTREES.md`, `GO-ENV.md`, `TOOLCHAIN.md` |
| `.github` | `workflows/ci.yml`, which runs exactly `make check` and nothing more; the BUILD.md-shaped issue templates; the Evidence-section PR template; and `CODEOWNERS`, which the §10.1 protected-mode topology in `docs/DEPLOY.md` depends on |
| `WORKFLOW.md` | BEN's own dogfood workflow config (see Dogfooding) |

## Canonical commands

```sh
make check    # everything CI runs: fmt-check, vet, staticcheck, race+coverage tests,
              #   workflow-check, worktree-check
make test     # go test ./...
make lint     # staticcheck (pinned version, via go run — no install step)
make fmt      # gofmt -w
make dist     # the SPEC §11 static binary, to bin/ben-<goos>-<goarch>. Defaults to
              #   linux/amd64 — the platform deploy/ben.service is for; override
              #   with DIST_GOOS/DIST_GOARCH
go run ./cmd/ben config effective WORKFLOW.md   # validate + inspect a workflow config
go run ./cmd/ben run WORKFLOW.md                # daemon; supervised only until the gates below close
go run ./cmd/ben status WORKFLOW.md             # what a daemon for that config is doing
go run ./cmd/benchreport session.json           # the #62 adapter comparison (docs/BENCH.md)
```

`make check` green is the definition of green — CI runs exactly this target, nothing more.

## Go on a dev machine

One rule, because one class of bug follows from breaking it:

> **Do not persist a Go setting you also export.** Pick the file or the shell, never both.

`go env -w` writes a file that governs **every** `go` invocation on the machine; an `export` in
`~/.zshrc` overrides it only in an interactive shell. Cron, launchd, systemd, CI, an editor's
language server and BEN's own hooks all see the file, so the two can disagree for years while
every shell you look at says the setting is fine.

**Audit it. This is the check, and it takes one line:**

```sh
cat "$(go env GOENV)"                       # what is persisted, if anything
env -i PATH="$PATH" HOME="$HOME" go env GO111MODULE GOFLAGS GOPROXY GOTOOLCHAIN
```

The second is the one that matters: `env -i` is roughly what a daemon gets.

**What belongs in that file:** machine-wide policy with no per-shell variation — `GOPRIVATE` for
private module paths, a corporate `GOPROXY`. **What does not:** `GO111MODULE` (default since
1.16; setting it is legacy and `off` is what bit us), and anything you would rather state
per-project. `GOFLAGS` is the sharpest of these — a persisted `-mod=vendor` or `-tags` silently
changes what every build in every context compiles.

[docs/GO-ENV.md](docs/GO-ENV.md) has the rest: which file, who reads it, the dogfood run this
cost, and why BEN's hook detects the misconfiguration instead of overriding it.

## Toolchain

`go.mod` declares **`go 1.26`** — a *minor-level* floor, deliberately, not the patch level
`go mod init` wrote. Any 1.26.x toolchain builds and checks this repo, offline and under
`GOTOOLCHAIN=local`, and CI resolves its version from that same line (`go-version-file: go.mod`)
rather than pinning one of its own.

Two rules follow, and `internal/arch`'s policy tests fail until either change is recorded
deliberately:

> **Raising the floor to a new minor (`go 1.27`) is ordinary; naming a patch needs an argument** —
> which runtime or compiler fix, and why — written in `go.mod`, where the next blocked contributor
> will look. A patch-level directive is a toolchain download for a contributor on `GOTOOLCHAIN=auto`
> and a hard refusal on `local`, and CI is structurally incapable of reporting either.

> **Adding a `toolchain` preference needs the same argument.** The repository carries none, and
> `internal/arch` enforces its absence.

[docs/TOOLCHAIN.md](docs/TOOLCHAIN.md) has the measured resolution table behind both, and the
rejected `toolchain go1.26.1` (#110).

## Working in worktrees

Ticket work happens on a branch in a linked worktree (see Dogfooding for the PR flow):

```sh
git worktree add ../ben-<topic> -b <branch>
```

Two rules follow, then a note about the work git does on its own. The first rule is correctness;
the second is hygiene, and the section says which is which because conflating them once cost a
morning.

### One branch, one worktree

> **A branch is checked out in exactly one worktree.** The primary checkout stays on `main` and
> is never a work surface. Never check out `main` — or any branch another worktree holds — in a
> linked worktree, never reuse one worktree across branches, and never pass
> `--ignore-other-worktrees`, `-B`, `-C`, or `git branch -f`.

**`-B` is the one that bites, because plain `checkout` protects you and `-B` does not.** Measured
on git 2.54: `git checkout shared` refuses while another worktree holds `shared`, and
`git checkout -B shared HEAD` takes the ref anyway, exit 0 and no warning. `git switch -C` and
`git branch -f` do the same; only the un-forced forms refuse. So the *safe* form fails loudly and
the *forcing* form succeeds silently, and the forcing form is what anybody reaches for when a
throwaway verification branch already exists. Prefer `git switch --detach <ref>` when the intent
is to look at a commit rather than to hold a branch — it touches no ref, so it cannot take one
from anybody. What it costs the other worktree is not a conflict but a *coherent older tree*: it
compiles, `make check` can be green on it, and `make worktree-check` — not the test suite — is
what detects the duplicate that caused it.

**If it has already happened, the repair has rules of its own, and each is a way to lose the work
or the evidence** — [docs/WORKTREES.md](docs/WORKTREES.md) carries what they were measured from:
the git output, the 2026-08-12 incident, and the procedure in full.

- **Diagnose before touching anything**: every repair below normalizes the tree and destroys what
  would have said what it was. Read per-file mtimes and the *linked* worktree's own reflog
  (`.git/worktrees/<name>/logs/HEAD`), not the primary's; prove the tree equals a commit
  (`git diff --quiet <c> && git diff --cached --quiet <c>`) against recent `origin/main` commits
  rather than counting changed paths; if none matches, stop — the tree contains local work;
  and never run `git stash create` while investigating — its dangling `WIP on <branch>` commit
  reads as evidence afterwards.
- **Clear the duplicate first** — `git -C <other> switch --detach`, or `git worktree remove` on a
  worktree holding no work of its own — or the repair is refused and this worktree is stranded on
  whatever it switched to.
- **Preserve before you reset, chained**, so a failed preservation cannot fall through to the
  destructive step: `git stash -a && git reset --hard HEAD`. `-a`, not `-u`: only `-a` parks the
  ignored files `reset --hard` overwrites at exit 0 (it also sweeps `.scratch/` and `.claude/`).
- **Never enumerate the at-risk paths yourself** — `reset --hard` destroys through prefix
  collisions that comparing `git ls-files --others` against the target tree reports nothing
  about. Either `git stash -a`, or let `git checkout <target>` refuse and name every file at risk.
- **Reset to `HEAD`, never to `origin/main`**: where the two differ, the second moves the
  **shared** ref while updating only this worktree's tree — the original hazard, from the other
  direction. `git pull --ff-only` is the repair when the ref itself is behind, once the duplicate
  is cleared.

### Put the worktree outside the repository

`.claude/` is gitignored, so a worktree nested there is invisible to git and to gitignore-aware
search, while every tool that walks the filesystem plainly (editors, `find`, indexers) sees a
second full copy of the tree per worktree. A sibling directory costs nothing and keeps that from
compounding. **Location alone is hygiene, not correctness** — unlike the rule above:
`internal/arch` skips dotted directories by name and nested modules by their `go.mod`, so a
worktree in either place is already out of scope
([docs/WORKTREES.md](docs/WORKTREES.md#where-a-worktree-lives)).

Note for Claude Code: `EnterWorktree` with a `name` creates under `.claude/worktrees/` and the
location is not configurable, so create the worktree with `git worktree add` first and enter it
by `path`.

### git keeps working after the command returns

`fetch`, `commit` and `merge` start auto-maintenance, and git **detaches** it: work outlives the
command in the shared object store, holding `gc.pid` and pack locks. `make check` went red on a
`TempDir` cleanup racing exactly that (#154).

> **Every git BEN invokes carries `-c gc.auto=0 -c maintenance.auto=false`** — one place,
> `gitArgv` in `internal/workspace/git.go` — because the daemon must account for every process
> touching a workspace it owns (SPEC §9.10). Your shell has no such guard; [docs/WORKTREES.md](docs/WORKTREES.md#what-a-detached-maintenance-run-costs)
> has the measurements.

## Definition of done

1. The ticket's acceptance criteria exist as tests (table-driven where possible).
2. `make check` is green. Paste its tail in the PR body — evidence over claims (SPEC §3.5)
   applies to us, not just to BEN's agents.
3. No new dependencies beyond SPEC Appendix A's set without human sign-off in the ticket.
4. Import boundaries hold (enforced by `internal/arch`): `internal/core` imports stdlib
   only; third-party boundary deps (yaml, liquid, go-github, fsnotify) are each owned by
   exactly one package. When a new adapter legitimately needs a dep, extend the allowlist
   in `internal/arch/arch_test.go` deliberately, in the same PR, with a comment citing the
   ticket.

## Conventions

- Strict at load, contained at run: config/template errors refuse loudly at startup; a
  render-time failure fails only that run.
- Named error values (`ErrX`) for every refusal the spec names; tests assert on them.
- The core never sees a raw agent event or provider payload — adapters translate to the
  closed enums/models at the boundary (SPEC §3.6).
- Comments state constraints the code can't show (usually with a SPEC § reference), not
  narration.
- A fake in `internal/fake` models the adapter it stands in for, and its contract is read off
  that adapter's control flow — the order its steps fail in, what each error path returns, what
  is genuinely shared rather than per-key. A fake that invents a guarantee the real component
  does not make is worse than a missing test: it lets code that depends on the invention pass.
  (`Workspaces.PublishFacts` refuses unscripted evidence for this reason — the zero
  `core.PublishFacts` is "the branch does not exist", a verdict of its own.)
- A test driven by the declaration it checks proves declared entries behave correctly, not
  that the declaration is complete. For closed-set or safety-critical contracts, pair it with
  an assertion anchored at an independent boundary that remains unchanged if an entry is
  omitted or misspelled. (#52 hit this five times over: a redaction test driven by each kind's
  `SensitiveFields` could not see the declaration shrink; `agenttest.Contract`'s single
  `Credential.Key` silently under-covered the adapter with two; and the credential table needed
  its provider-key half, its env half, and its sensitivity half each anchored somewhere else —
  parsing, the §7.6 reservation, and injection respectively.)

## Dogfooding

`WORKFLOW.md` is BEN's own workflow. `ben run` is assembled (B11), and B10 startup recovery has
landed (#8, `707b8a7`), so §9.10 reconstructs the principal's claims after an interruption.
Unattended dogfooding remains gated on [issue #76](https://github.com/srhg-ai-7cef3f93/ben/issues/76); its one open lane is the **forge
control** (§10.1 requirement 3). Branch protection is applied and read back ([#83](https://github.com/srhg-ai-7cef3f93/ben/issues/83)).
[#155](https://github.com/srhg-ai-7cef3f93/ben/issues/155) and [#156](https://github.com/srhg-ai-7cef3f93/ben/issues/156) landed the mechanism: an independent claim assignee and
`credential_sources` whose tracker, base-fetch and publish credentials are minted at need under
distinct workload identities. **The remaining step is deployment**: this repository's workflow
still names the legacy token, so BEN publishes as whoever runs the daemon until an operator
configures both Octo sources per [docs/DEPLOY.md](docs/DEPLOY.md). Until then, runs are supervised
and `deploy/ben.service` remains blocked by its `ExecStartPre` gate.
Conventions that make dogfooding work:

- **`WORKFLOW.md` must declare `deployment.mode`** (SPEC §5.2.9, §10.1) — `protected`,
  `risk-accepted` or `attended`. There is no default: omission fails `Load`, so `ben run` and
  `ben config effective` both refuse, and `make workflow-check` goes red. **This breaks every
  pre-#128 workflow file**, deliberately: §10.1 forbids arriving in an unattended mode by
  omission, and a default is arrival by omission with extra steps. BEN verifies none of the
  declared properties — §10.1 owes that to the deployment — so the declaration is an assertion
  BEN records, not one it checks. It is process-lifetime: a reload that changes it is refused and
  a restart adopts it.
- Label **`ben-queue`** on an issue marks it dispatchable to BEN. Filed-but-unlabeled tickets are backlog.
- Labels **`ben:*`** (`ben:claimed`, `ben:running`, `ben:needs-review`, `ben:failed`) are BEN's state projection (SPEC §9.3) — never set or remove them manually.
- `make workflow-check` load-validates `WORKFLOW.md` with BEN's own loader; CI catches schema drift without credentials, network access, or a harness.
- Issues follow the BUILD.md ticket shape (see `.github/ISSUE_TEMPLATE/`); PRs carry an Evidence section (see `.github/PULL_REQUEST_TEMPLATE.md`) and `Fixes #<n>`.
- **Adapters are compared only on #62's fixed cohort, never the dogfood queue.** Runs use distinct canary issues/workspaces and cannot inform daemon decisions; see `docs/BENCH.md`.
