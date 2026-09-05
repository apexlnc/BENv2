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
| `cmd/ben` | CLI entry point and assembly for `ben config effective`, `ben run`, and `ben status`. The place components that may not import each other are bound together — including the §9.7 checker and the loop that routes it (`verifier.go`), and the v2 dispatch leg that binds the backend, the seams, the workspace strategy, the daemon-side evidence store and the provider argv (`remote.go`, #205) (SPEC §11) |
| `internal/core` | Shared interfaces and closed enums (SPEC §6–8). Stdlib-only — imports nothing else |
| `internal/config` | Strict `WORKFLOW.md` loader and hot-reload watcher (SPEC §5) |
| `internal/template` | Strict Liquid template layer (SPEC §5.6) |
| `internal/registry` | The one kind table: `tracker.kind`/`agent.kind`/`credential_sources[].kind` → registered kind, asked by both the loader and `ben config effective` (SPEC §5.7, §11) |
| `internal/credential` | The credential source kinds behind `credential_sources` (SPEC §5.2.10, §10.2): `octo_sts`, direct `projected_oidc`, and `static` for compatibility and development. Each kind's `Describe` is **pure** — no network, no filesystem, no instance — because it is what load-time validation and §5.4's reload comparison call, which is also what keeps `make workflow-check` credential-free. `credtest` is the conformance suite all three kinds run unmodified, so a future `github_app` inherits the proof |
| `internal/tracker/github` | GitHub tracker adapter: read kernel + closed write set (SPEC §8), plus the GraphQL content and remote-PR fact reads REST cannot answer |
| `internal/gitcmd`, `internal/gitremote` | Shared Git boundaries: every invocation carries the no-background-maintenance overrides and child environment (#154), plus the neutralization of the surfaces a run can write inside `base.git` to steer a daemon-side git — hooks, fsmonitor, replace refs and legacy grafts (#228) — while stdlib-only remote syntax checks identically refuse credentials and transport helpers before either Git driver acts. `gitremote` also owns the **one** credential helper both drivers install (#230): it parses git's request and answers only for the protocol and host of the remote that invocation was built for, so a redirect or an `insteadOf` rewrite to another host gets silence rather than the token — shared rather than copied, because a per-package copy is what silently stops scoping |
| `internal/workspace` | Git worktree lifecycle, safety invariants, hooks, the durable claim-epoch/base/target store, and publish-evidence git facts (SPEC §6, §9.7) |
| `internal/agent/harness` | Shared process-per-attempt runtime behind both agent adapters: lifecycle, liveness, execution-domain boundary, child-environment composition, transcripts (SPEC §7.2–§7.6). `lifecycle.go` is the pure decision layer — which terminal event, and when it may be published; `handle.go` is the I/O driver around it; `timings.go` is the one seam for its windows, and `bound.go` the one discipline for its sizes — what is held of an untrusted child's output, with every cut stated (#235) |
| `internal/agent/claudecode` | claude-code runner adapter: provider block, argv, stream translation, readiness probes (SPEC §7.7) |
| `internal/agent/codexexec` | codex-exec runner adapter: the same, for `codex exec --json` (SPEC §7.7) |
| `internal/agent/agenttest`, `internal/agent/runnertest` | The local AgentRunner conformance suite both adapters run unmodified (SPEC §7.1–§7.6), and its universal `core.RunHandle` subset run by the local and remote fakes. Their source audits keep local OS facts out of the universal cases (#192) |
| `internal/remote`, `internal/remote/remotetest` | The directly constructed v2 execution-substrate boundary and its scripted fake (#192, #46): opaque workspaces; sandbox-scoped, request-digested durable processes; recoverable event delivery and replay proofs; independent stream/process/domain evidence; and restart-safe hooks. No HTTP, config key, provider schema or second control loop: `internal/airlock` is the backend foundation that lands against it (#194). See `docs/REMOTE.md` |
| `internal/airlock`, `internal/airlock/airlocktest` | BEN's opt-in v2 backend foundation behind those seams (#194): the stdlib HTTP client for Airlock's frozen v2 contract, the two durable addresses that contract deliberately does not answer — which sandbox a claim acquired, which run a dispatch resolved to — the end-of-claim disposal gated on domain quiet, startup reconciliation, and an in-process server faithful enough to script lost responses, restarts, replay, gaps and cross-tenant collisions. `internal/remotews` and the `cmd/ben` routing below are what make it dispatchable (#205). Stdlib-only, enforced by `internal/arch`. See [AIRLOCK.md](docs/AIRLOCK.md) |
| `internal/remotews` | SPEC §6.1's workspace strategy for a claim running on a v2 substrate (#205) — the **sibling** of `internal/workspace`, not a layer over it: neither imports the other's lifecycle, and this one reads nothing inside the sandbox, because everything there is authored by the thing being judged (SPEC §3.5). Its **two clocks** are the design: the *workspace cycle* (repository + issue + the standing approval event) selects the sandbox and outlives an assignment, so a reassignment inside one approval revises the same tree; the *verification epoch* stays the assignment and pins the trusted base and target #193 measures against. Revocation then reapproval is a new cycle — a different address, so attaching the retained sandbox is unexpressible rather than discouraged — and #266 gives every replaced cycle its own durable disposal address so a downtime reapproval cannot strand it or let its cleanup act on the replacement. See [REMOTE.md](docs/REMOTE.md) |
| `internal/verify`, `internal/mirror` | The local and remote publish-evidence checkers behind `done` (SPEC §9.7), selected without mixing their evidence. Remote verification reads the claim-time base/target pin and canonical branch head from a daemon-side bare store no run can reach; `mirrortest` covers the real and fake stores unmodified (#193) |
| `internal/orchestrator` | The authority loop: states, tick, dispatch, retry, reconciliation (SPEC §9) |
| `internal/fake` | In-memory tracker/workspace/runner + manual clock, shared by the orchestrator's tests, `cmd/ben`'s end-to-end acceptance tests, and B12. Not test files — several packages need them, so a fake's fidelity to the adapter it stands in for is a correctness concern (see Conventions) |
| `internal/state` | The §10.3 white-box state dir: run records, the persisted §9.11 transition log, the per-attempt outcome log behind `ben status`'s aggregate (#60), and where transcripts go. Read by a *different process* than the one writing it, which is why every file here is replaced by rename or appended one whole record at a time — `jsonl.go` is the single append-only writer both logs use, and `internal/orchestrator/durable.go` the single off-loop queue that feeds them |
| `internal/integration`, `internal/scenario` | B12's §12.3 invariant suite plus #220's strict JSON scenario vocabulary and byte-stable trace renderer. The loop, loader, watcher, template layer and §9.7 checker are real; only the world outside the process is faked, so both run in CI with no network, subprocesses or wall-clock waits. Scenario code describes diagnostics only; integration is its sole binding to the authority loop and conformant fakes (`docs/SCENARIO-LAB.md`). `internal/integration/doc.go` carries the §12.3 coverage map — including rows deliberately asserted at another package's boundary |
| `internal/bench`, `cmd/benchreport`; `internal/ticketprep`, `cmd/ticketprep`, `.agents/skills/prep-ticket`, `.claude/skills/prep-ticket` | Developer-only analysis surfaces unreachable from the daemon: #62's fixed cohort comparison (`docs/BENCH.md`), and #222's shared explicit-only `prep-ticket` workflow (`$prep-ticket` in Codex, `/prep-ticket` in Claude Code), which binds exact issue content to committed Git facts and safely renders bounded advice ([TICKETPREP.md](docs/TICKETPREP.md)) |
| `internal/review`, `internal/reviewctl`, `internal/reviewrun`, `cmd/benreview` | The #11 review controller, which #204 moved into the daemon (`docs/REVIEW.md`). `internal/review` is the unchanged pure per-issue reducer, the marker vocabulary and the validation behind every route — still the policy authority. `internal/reviewctl` is the trusted half that holds the controller credential, captures the exact base/head subject itself, validates one closed verdict and makes the bounded forge writes; `internal/reviewrun` is the substrate-neutral reviewer-execution boundary behind it — a credential-stripped local child or one durable Airlock run in the workspace-cycle sandbox — which publishes nothing and holds no forge credential. There is no repository workflow: the daemon's sweep is the availability mechanism and `cmd/benreview` is the operator's dry-run/reconcile window, which never invokes a model. The orchestrator neither imports nor is imported by the reducer: they meet only at SPEC §8.4's published milestone in and a `COMMENT` review plus one unassignment-or-revocation out. The forge client is stdlib rather than go-github deliberately — `internal/tracker` owns that dep |
| `internal/arch` | Structural test enforcing the import boundaries below |
| `internal/partest` | What keeps `make check`'s test time down without weakening it (#167): a gate bounding how many of a package's tests run at once, and the source audit deciding which may join one. The bound is the design — these suites drive real child processes, and unbounded `t.Parallel` would trade elapsed time for the load flakes they exist to expose. Used by `internal/agent/agenttest`, both adapters, and `internal/workspace` |
| `deploy/ben.service` | Sample systemd unit. `KillMode=mixed`, `TimeoutStopSec` and the §10.1 mode statement are load-bearing — `cmd/ben`'s test holds the file to what the daemon claims about it |
| `scripts` | The §12.4 real-integration smoke runner and exact workflow, #62's load-validated adapter/model benchmark profiles, and #194's credential-gated Airlock kind smoke |
| `docs` | Long-form references kept out of the root, because this guide is loaded into every agent's context and they are not. For an operator: `DEPLOY.md`, the §10.1/§10.2 unattended-operation runbook — the account, the credentials, the branch protection the review gate rests on; and `SMOKE.md`, the §12.4 smoke profile — one issue end to end against a canary repo, the one check that needs credentials and network; and `BENCH.md`, the #62 adapter benchmark — the cohort, the per-cell procedure, and how to read the comparison; and [`REVIEW.md`](docs/REVIEW.md), the #11/#204 review controller — its identities, its markers, its two substrates and two clocks, what it is structurally unable to do, and how to turn it on, with [`REVIEW-GUIDANCE.md`](docs/REVIEW-GUIDANCE.md) the deployment's own standard for what counts as a finding. For a contributor, [SCENARIO-LAB.md](docs/SCENARIO-LAB.md), #220's deterministic replay format and trust boundary; [TICKETPREP.md](docs/TICKETPREP.md), #222's offline advisory artifact; plus the evidence behind four rule sections, each linked from the section that states the rule: `WORKTREES.md`, `GO-ENV.md`, `TOOLCHAIN.md`; and [REMOTE.md](docs/REMOTE.md), the direct-construction v2 seams and unchanged v1 configuration boundary. For both: [AIRLOCK.md](docs/AIRLOCK.md), the #194 Airlock foundation — its `substrate:` declaration, its fourth credential identity, what it persists and what it refuses |
| `.github` | `workflows/ci.yml`, which runs exactly `make check` and nothing more, and `workflows/publish-daemon-image.yml`, which builds, smokes and publishes the daemon image to ECR per commit (#180/#181) — the only two workflows: #204 retired the #11 reviewer's `reviewer/` prompt, wrapper and uninstalled workflow, and `internal/reviewctl` asserts none of them came back, because a workflow is arbitrary code holding repository credentials. Also the BUILD.md-shaped issue templates; the Evidence-section PR template; and `CODEOWNERS`, which the §10.1 protected-mode topology in `docs/DEPLOY.md` depends on |
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
go run ./cmd/benreview -repo o/r -issue 11 -dry-run   # the #11 review controller (docs/REVIEW.md)
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
> `gitcmd.Argv` in `internal/gitcmd` (both daemon Git drivers and ticketprep share it), because BEN must account
> for every process touching a repository it owns (SPEC §9.10). Your shell has no such guard;
> [docs/WORKTREES.md](docs/WORKTREES.md#what-a-detached-maintenance-run-costs) has the measurements.

## Definition of done

1. The ticket's acceptance criteria exist as tests (table-driven where possible).
2. `make check` is green. Paste its tail in the PR body — evidence over claims (SPEC §3.5)
   applies to us, not just to BEN's agents.
3. No new dependencies beyond SPEC Appendix A's set without human sign-off in the ticket.
4. Import boundaries hold (enforced by `internal/arch`): `internal/core` imports stdlib only,
   and **every third-party source import plus every direct `go.mod` requirement carries exactly one
   recorded ownership decision** — a single owner or explicit unrestricted reason. `// indirect`
   is no exemption. When a ticket legitimately needs a dependency, record that decision in
   `internal/arch/arch_test.go` in the same PR, with a comment citing the ticket.

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
The unattended-dogfood gate, [issue #76](https://github.com/srhg-ai-7cef3f93/ben/issues/76), **closed on 2026-08-20**: branch protection is applied and
read back ([#83](https://github.com/srhg-ai-7cef3f93/ben/issues/83)); [#155](https://github.com/srhg-ai-7cef3f93/ben/issues/155) and [#156](https://github.com/srhg-ai-7cef3f93/ben/issues/156) landed an independent claim assignee and
`credential_sources` minted at need under distinct workload identities; the deployed canary publishes
as a bot the reviewing human is not, with the four behavioural probes waived on record. **What that
closure does not grant is an unattended mode**: this repository's committed workflow still declares
`attended` and falls back to the legacy token, so a daemon run from it publishes as whoever runs it.
Moving a deployment to `risk-accepted` is the explicit decision [docs/DEPLOY.md](docs/DEPLOY.md) describes; unattended
dispatch on public input also waits on [#195](https://github.com/srhg-ai-7cef3f93/ben/issues/195). Until then, local runs are supervised and `deploy/ben.service` remains blocked by its `ExecStartPre` gate.
Conventions that make dogfooding work:

- **`WORKFLOW.md` must declare `deployment.mode`** (SPEC §5.2.9, §10.1) — `protected`,
  `risk-accepted` or `attended`. There is no default: omission fails `Load`, so `ben run` and
  `ben config effective` both refuse, and `make workflow-check` goes red. **This breaks every
  pre-#128 workflow file**, deliberately: §10.1 forbids arriving in an unattended mode by
  omission, and a default is arrival by omission with extra steps. BEN verifies none of the
  declared properties — §10.1 owes that to the deployment — so the declaration is an assertion
  BEN records, not one it checks. It is process-lifetime: a reload that changes it is refused and
  a restart adopts it.
- Label **`ben-queue`** marks an issue dispatchable to BEN; filed-but-unlabeled tickets are backlog. **Only a human applies it** — SPEC §9.5's approval act — and it stays standing through every revision round. The #11 review controller may only *remove* it: revocation, which asserts nothing ([docs/REVIEW.md](docs/REVIEW.md)).
- Labels **`ben:*`** (`ben:claimed`, `ben:running`, `ben:needs-review`, `ben:failed`) are BEN's state projection (SPEC §9.3) — never set or remove them manually.
- `make workflow-check` load-validates `WORKFLOW.md` with BEN's own loader; CI catches schema drift without credentials, network access, or a harness.
- Issues follow the BUILD.md ticket shape (see `.github/ISSUE_TEMPLATE/`); PRs carry an Evidence section (see `.github/PULL_REQUEST_TEMPLATE.md`) and `Fixes #<n>`.
- **Adapters are compared only on #62's fixed cohort, never the dogfood queue.** Runs use distinct canary issues/workspaces and cannot inform daemon decisions; see `docs/BENCH.md`.
