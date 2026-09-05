# BEN — Build Plan

Companion to [SPEC.md](SPEC.md) (v1, locked 2026-08-06). Twelve tickets, each sized for one
focused effort and written to be filed as a GitHub Issue verbatim (title = heading, body =
the rest). `Depends on` lines map to native blocked-by relations.

**Suggested milestones**

- **M1 — first dispatch:** B01 → B02 → B04 → B05 → B06 → B08. At the end, a labeled issue on
  a real repo goes claim → worktree → Claude Code run → verified PR → `done`.
- **M2 — hardening:** B03, B07, B09 (full evidence matrix), B10. Retry, recovery, reload,
  and the second harness.
- **M3 — operability:** B11, B12. CLI polish, logging, and the invariant scenario suite.

Interfaces-first: B01 defines the Go types/interfaces for §6–8 of the spec so B04/B05/B06
proceed in parallel against them.

---

## B01 — Config loader and workflow contract

**Spec:** §5.1–5.5, §5.7, §5.8 · **Depends on:** —

Go module scaffold plus the `WORKFLOW.md` loader: front-matter split, `yaml.v3` with
`KnownFields(true)`, the closed top-level key set with the two opaque carve-outs
(`tracker.provider`, `agent.provider`), `version` handling, defaults table, `$VAR` secret
resolution (empty = missing), path normalization, `workflow_key` derivation. Define the
shared interface types (`WorkspaceProvider`, `AgentRunner`, `TrackerAdapter`, `Event`,
`Issue`, `RunSpec`) so downstream tickets build against them. Includes
`ben config effective [--json]` with per-field provenance and mandatory redaction.

**Acceptance**
- Strictness matrix passes: unknown key anywhere non-opaque fails; opaque blocks pass through.
- Empty prompt body, non-map front matter, bad `version` all refuse with named errors.
- `config effective` shows provenance (default/file/env) and never prints a resolved secret.

## B02 — Strict template layer

**Spec:** §5.6 · **Depends on:** B01

Liquid rendering with load-time strictness: an AST walk validating every referenced variable
against the closed set (`issue`, `attempt`, `workspace`, `run`) and rejecting unknown
filters; render-time strict check as backstop. Resolve the flagged risk: prove
`osteele/liquid` supports the walk (or wrap/replace it) and record the outcome in the ticket.

**Acceptance**
- `{{ issue.titel }}` and unknown filters fail at **load**, not first render.
- The canonical publish snippet renders correctly for first attempt, continuation
  (`previous_outcome == "succeeded"`), and failure retry.

**Risk resolved (2026-08-06).** `osteele/liquid` v1.8.1 supports the walk — no wrap or
replacement needed, with one supplement:

- `Template.GetRoot()` exposes the parsed tree (`SeqNode`/`BlockNode`/`ObjectNode`/`TagNode`),
  each node carrying its tag name, raw expression text, and source line. Expressions compile
  to opaque closures, so `internal/template` lexes each node's expression itself (mirroring
  the engine's scanner rules; asserted against engine behavior in tests) and validates paths
  — properties included, with `assign`/`capture`/`for` scope tracking — against per-variable
  shapes. `{{ issue.titel }}` is a load error; filter output is the checking frontier.
- Unknown filters error at render by default (v1.8+); load-time rejection probes the engine
  per filter name and treats only an `expressions.UndefinedFilter` cause as unknown, so the
  check cannot drift from the filter registry.
- `Engine.StrictVariables()` is the render-time backstop. It conflates null with undefined:
  known-but-null values (`{{ attempt }}` on first attempt) emitted unguarded fail that
  render. Documented; the canonical snippet's guarded pattern is the contract.
- Unknown tags already fail at parse; `{% include %}` is rejected at load as unsupported.
  Caveat: `render`/`parser` are internal-ish subpackages with no compatibility guarantee —
  version pinned, walk covered by tests that fail loudly on upgrade.

## B03 — Hot reload

**Spec:** §5.4 · **Depends on:** B01

fsnotify watch on the parent directory, ~200 ms debounce, keep-last-known-good, and the
block-new-dispatches-on-invalid-reload state with loud operator error. Wire the defensive
pre-dispatch revalidation hook the orchestrator (B08) calls.

**Acceptance**
- Editor-style atomic save (write temp + rename) triggers reload.
- Invalid reload: in-flight config object untouched, dispatch-blocked flag set, error logged;
  fixing the file clears it without restart.
- A reload that changes an adapter's provider block **reconstructs that adapter and re-checks
  `Ready` before it is used** (assembly decision 13). Reusing the old instance would operate
  under a configuration its readiness check never saw. In-flight runs keep the instance they
  started with, per §5.4.

## B04 — GitHub tracker adapter

**Spec:** §8 · **Depends on:** B01

The read kernel and closed write set against `go-github`: normalized `Issue` (incl.
`created_at`, native blocked-by → `blockers`), `dispatchable` computation (required labels,
active state, any-assignee exclusion, open blockers, no `ben:*` state label),
ETag-conditional `Fetch`, `FindPR`, `Claim` with post-write read-back verification,
`Release`, `SetStateLabels`, `Comment`, the per-issue reconciliation read `Get`, plus the two
cache-bypassing recovery reads `ClaimedByPrincipal` and `ClaimHistory` (§8.2, assembly
decision 11) and the conditional `HeldClaims` sweep read (decision 14). Rate-limit handling
honoring `Retry-After` and `X-RateLimit-Reset`; Search API kept out of all poll paths.
Validation rejects empty `required_labels`.

**Acceptance**
- Registers as a `TrackerKind` (assembly decision 13): `Structural(TrackerConfig)` is pure
  and takes the core-owned fields alongside the opaque block; `Ready(ctx)` owns credential
  resolution and reachability. An omitted `token` is structural-clean and fails at `Ready`;
  `token: $UNSET` is a load error (§5.5).
- Claim race (fixture: assignment 201 but read-back shows another assignee) → resolved from
  the assignment log, and the release removes **only** this daemon's principal — a
  co-assignee, human or daemon, survives the release untouched.
- Contested-claim ordering is stable under same-second timestamps: two `assigned` events in
  the same second are ordered by event id, and every claimant computes the same winner.
  Asserted at `-count` > 1, since the first version of this fix passed once and failed at 15.
- A login the log has seen and since released is skipped (a withdrawing loser must not leave
  the race winnerless); a login the log never saw is unorderable and makes every claimant
  yield rather than guess.
- `ClaimHistory` normalizes to the closed `ClaimEvent` shape ordered by `(at, id)`, carrying
  `subject` for both assignee logins and label names — recovery cannot distinguish a `done`
  projection from a human re-queue without it. The raw go-github event payload never escapes
  the adapter (invariant 6).
- `ClaimedByPrincipal` applies **no** label or state filter, and a test asserts it returns a
  closed issue and a de-labeled one — the two cases a queue-shaped fetch would drop.
- `ClaimedByPrincipal` and `ClaimHistory` bypass the conditional cache — asserted via the
  request recorder, since a cached answer would attest to nothing.
- **Assigned-to-self is not dispatchable either** (§8.3): a published issue awaiting review is
  assigned to our own principal, and dispatching over it would redo finished work. A rule that
  excludes only *other* parties MUST fail this test.
- `ClaimHistory` carries `closed` and `reopened` (decision 14). A fixture where the issue was
  closed and reopened yields both, in `(at, id)` order, so the close is still evidence after
  the reopen — the fact current tracker state cannot express.
- **`revision` moves across a same-second close-and-reopen** (§8.3). Every timestamp in the fake
  is one second, so a token derived from `updated_at` alone MUST fail this test — and it is
  stable across two reads of an unchanged issue, or the sweep's trigger would fire every tick.
- **The projection is exhaustive, and enforced by its type — not by a list of things not to
  hash.** The token derives from a closed struct carrying exactly the three projected values;
  one function maps the provider payload onto it, and the hash sees only the struct, so it has
  no wider field to reach. A test pins the type's fields, which covers **every** field anyone
  might add rather than the ones a table remembered: adding `milestone` to the hash MUST fail
  it. Behavioral coverage stays alongside it — `title`, `body`, `labels`, `assignees`, comment
  count, `locked`, and the URL leave the token unmoved; `state`, `state_reason`, and
  `updated_at` move it — but the negative rows document the semantics, they no longer carry the
  guarantee. Two earlier versions of this criterion were too weak in exactly that way: a token
  hashing the title passed a test that mutated only labels, and a `milestone` term passed a
  table of seven named fields.
- **The premise that licenses the exclusion is asserted too:** a pulled label is visible in the
  sweep response itself, so §9.8's partition rule sees it without the token moving. If that ever
  fails, the projection is no longer sufficient and §8.3's MUST has to widen with it.
- `HeldClaims` issues the same query as `ClaimedByPrincipal` but **is** conditional: a repeat
  call revalidates and costs no core-budget request, asserted via the request recorder and the
  cache's hit counter. The two are separate methods precisely so this posture cannot leak into
  the recovery read.
- Milestone comments are idempotent **per projection occurrence**, and the occurrence is
  per kind (§8.4). Two directions must both hold, and no single key satisfies both: a
  `needs-review` issue re-queued and failed again posts a *second* `needs-review` comment
  (same claim cycle, different occurrence — cycle-keying fails this), while `ben:claimed`
  cycling through preparing/verifying/backoff leaves exactly one `claimed` comment
  (per-transition keying fails this).
- 304 responses cost no core-budget requests (verify via request recorder).
- Label projection writes exactly the §9.3 set; comments only at the four milestones.

## B05 — Worktree workspace provider

**Spec:** §6 · **Depends on:** B01

Bare `base.git` bootstrap under the XDG layout, fetch-before-attempt, worktree create/reattach
on `ben/<workspace_key>` (never `-B`), sanitization + ≥64-bit hash suffix, the three
safety invariants (with symlink normalization), `Dispose(keep)`, startup sweep, per-issue
locks, and the four hooks with `timeout_ms`, cwd = workspace, and per-hook failure semantics.
Implement the normative failure taxonomy: `worktree list --porcelain` verification,
prune-and-retry-once, fail-closed on ambiguous git errors.

**Acceptance**
- Retry after a failed attempt reattaches the branch; agent commits from the prior attempt
  survive (explicit regression test against `-B`).
- Stale worktree registration and crashed-run debris are both recovered by prune-and-retry;
  an unrecognized git failure aborts loudly with the workspace kept.
- Hook matrix: `after_create`/`before_run` failures abort; `after_run`/`before_remove`
  failures are logged and ignored; all four fire at the specified points.

## B06 — AgentRunner interface and claude-code adapter

**Spec:** §7 · **Depends on:** B01

The runner contract (`RunnerKind.Structural`/`New`, then `Ready`, `Capabilities`,
`Start` → `RunHandle`; assembly decision 13) plus the first real
adapter: `claude -p --output-format stream-json`, event translation to the closed enum,
continuation token from `--resume` session IDs, stall + attempt timeouts, heartbeat
synthesis from raw stdout, raw transcript retention, per-run env injection (secrets never in
argv; allowlisted passthrough only), prompt via stdin, `Setpgid` process groups with
SIGTERM→grace→SIGKILL and confirmed/unconfirmed stop reporting, 10 MiB scanner buffer.

**Acceptance**
- Process exit without terminal event → `failed(crashed)`; silence past stall window →
  `failed(stalled)`; exit codes never decide outcome.
- `ps`-visible argv and child env audited in a test: no secret material, only allowlist +
  injected vars.
- Stop on a stubborn child (ignores SIGTERM) escalates to SIGKILL and reports confirmed;
  an unkillable (simulated) child reports unconfirmed.
- Registers as a `RunnerKind` (assembly decision 13). `Structural(provider)` is **pure** — a
  test asserts it passes with no harness on `PATH`, since `ben config effective` must work on
  a machine that has never installed one. `exec.LookPath` and the `--version` identity check
  move to `Ready(ctx)`, which is where a missing or skewed binary is a refusal.
- **Provider config binds at `New`; `Start` does not re-parse it from the `RunSpec`.** A test
  asserts `Ready` and `Start` cannot disagree: the binary `Ready` verified is the binary
  `Start` executes. A runner that took its provider block per-run would let readiness pass
  against one configuration and launch another.
- **`RunSpec.Env` accepts `BEN_`-prefixed keys only** (§7.6); anything else is
  `failed(launch_error)`, not a precedence decision. The table must cover the two cases a
  "no overriding provider-derived keys" rule would have let through: `ANTHROPIC_API_KEY`
  injected when the provider block **omits** `api_key` (nothing was derived, so nothing was
  protected), and `HOME`, which arrives via the allowlisted passthrough rather than the
  provider block yet redirects keychain and config-file credential lookup. Assert the
  refusal, never an ordering.
- **`Structural` rejects `BEN_`-prefixed keys in `provider.env` and `provider.env_passthrough`**
  — the other half of the reservation, and the worse half if left open, since a config-side
  collision is authored once and hits every run. It is a property of the file, so it fails at
  **load**, not at dispatch. Table rows for both surfaces; a namespace enforced on only the
  `RunSpec` side MUST fail them.

## B07 — codex-exec adapter

**Spec:** §7.7 · **Depends on:** B06

Second adapter: `codex exec --json`, proving the interface carries two real harnesses with
zero orchestrator changes. Documents its provider block and publish-credential surface;
declares capabilities honestly (resume, usage).

**Acceptance**
- The B06 adapter conformance test suite (shared, table-driven) passes unmodified.
- No orchestrator or interface diff in the PR beyond adapter registration.

## B08 — Orchestrator core

**Spec:** §9.1–9.6, §9.8–9.9, §9.11 · **Depends on:** B01, B02, B04, B05, B06 (buildable
early against fakes)

The single-mutator authority loop under errgroup: nine states with the closed transition map
(illegal transition = loud error), run-record fields `{stage, attempt, failure_reason}`,
tick sequence (reconcile → preflight incl. revalidation → fetch → sort → dispatch), FIFO
age-only dispatch, global concurrency cap, dual-track retry (continuation ~1 s with token;
backoff `min(10s·2^(attempt−1), max)` + deterministic FNV jitter), backoff-fire re-fetch
rules, reconciliation of running **and** parked issues, the held-claim sweep that releases
retained `done` claims (decision 14), interrupt-vs-stop with unconfirmed-stop claim retention,
budget enforcement → `needs-review`, label projection, milestone comments, transition log.
Ships the FakeRunner/FakeTracker/FakeWorkspaceProvider used by its own tests and B12.

**Acceptance**
- Transition-map test: every legal edge exercised; a fuzz of illegal edges all error.
- Race detector clean under concurrent watcher/timer/runner signal storms.
- Backoff sequence exactly reproducible (deterministic jitter) in tests.
- Unconfirmed stop: claim retained, no re-dispatch, retried next tick.
- A human who assigns themselves inside the claim write→read-back window loses the assignment
  order, so the claim verifies and dispatch proceeds alongside them (§8.4 known window).
  Reconciliation MUST **detect** the unroutable assignee set on the next tick and begin the
  stop, workspace kept. **Release is not bounded** and MUST NOT be asserted as next-tick: §9.8
  retains the claim whenever termination is unconfirmed and retries on later ticks, so a test
  demanding release-next-tick would encode a guarantee the design does not make. Their
  assignment is never removed.
- **A retained `done` claim is released by a running daemon**, not only by a restart: the issue
  closes, and the next tick's held-claim sweep releases and drops the record. The fixture MUST
  keep the daemon up across the close — a test that restarts asserts the old behavior.
- **Close-and-reopen inside one poll interval is still released.** The issue is closed and
  reopened between two sweeps, so it reads `open` when the sweep looks and only the log still
  says it closed. The moved **revision** is what buys the one `ClaimHistory` read; the `closed`
  event in the claim cycle is the verdict. It is then unassigned and unlabelled, which is to say
  dispatchable, and re-enters the queue as new work at `attempt 1`. Three implementations MUST
  fail this test: a sweep keyed on current tracker state; one that reads history **only for
  records the list shows closed** (the reopened issue reads open, so that shape can never
  discover the close at all); and one triggering on `updated_at`, since the fixture MUST put the
  close and the reopen **inside one timestamp second** (§8.3, §8.4). Scope: this covers a reopen
  the revision projection expresses. The case it cannot — a *repeated* reopen inside one
  second — is restart-coupled by §9.2 and belongs to B10's gate-1 test; it MUST NOT be asserted
  as a release here.
- **The sweep's cost does not scale with held claims.** N held `done` records cost one
  conditional `HeldClaims` request per tick, not N — asserted on the fake's request log. A
  per-record `Get` implementation MUST fail this test. N claims absent from the response at
  once cost the same number of confirming reads per tick as one, and the explicit rotation gives
  every stable candidate a turn even when one confirmation keeps failing. `ClaimHistory` costs
  one read per *observed change*: an idle tick over N records reads no history, a tick where one
  record's revision moved reads exactly one, and an ordinary close reads none — the list response
  settles that by itself. A trigger that fires on an unchanged revision MUST fail too: it puts the
  O(held) cost back through the back door.
- **A held claim stripped of its required labels is released** (mirrors the unroutable rule),
  and one **absent from the sweep read is confirmed with a `Get` before anything is dropped** —
  absence is not evidence. Sweep-read failure keeps every record and retries next tick.
- **Neither restart-coupled case releases on a tick** (§9.2). A PR closed unmerged leaves no
  event on the issue; a *repeated* close-and-reopen inside one timestamp second leaves the
  revision projection unmoved, so the token does not change and the sweep never
  looks. Both claims stand until a restart reclassifies them (§9.10). A test asserting release
  on a tick for either would encode a guarantee the design does not make.

## B09 — Verification and publish evidence

**Spec:** §9.7 · **Depends on:** B04, B05, B08

Claim-time base SHA recording in `Prepare`; the three-part evidence check (branch advanced +
descends, branch on origin, open PR via `FindPR`); routing: complete → `done` (dispose,
retain claim, publish comment), clean-exit-incomplete → continuation, contradiction or
exhausted turns → `needs-review`. Fail closed on verification errors.

**Acceptance**
- Agent pushes but PR creation fails → continuation re-dispatch prompts publish completion.
- Agent claims success with zero commits → `needs-review`, never `done`, workspace kept.
- Force-push/rewritten branch (not descending from base) does not verify.

## B10 — Startup recovery

**Spec:** §9.10, §6.4 · **Depends on:** B08, B09

The statelessness invariant realized: reconstruct from tracker + git on startup by
classifying every candidate from positive evidence per the §9.10 table (assembly decision
11) — fetch issues assigned to the claim principal, anchor on the claim-establishing
`assigned` event, and route each observed shape to its verdict. Terminal workspaces swept;
lost continuation tokens tolerated.

**Acceptance**
- Kill −9 during each of `claimed`/`preparing`/`running`/`backoff`, restart: no duplicate
  dispatch, no lost claim, orphan resumes on the same branch **once its run is confirmed
  gone**. The daemon's own death does not kill the agent — every attempt runs in its own
  process group and nothing supplies a parent-death signal — so a live previous run is the
  ordinary case here, not an edge.
- **A possibly-live workspace is not reattached, disposed, or released** (§9.10's workspace
  precondition). The record is retained and the question re-asked on later ticks; it converges
  with no human once the run ends. A classifier that acts on the projection table without
  consulting the marker MUST fail this test, and so MUST one that gates only the orphan row —
  gate 1's disposal-and-release and step 5's sweep are covered by the same rule.
- **A marker whose launch outcome is unknown parks `ben:needs-review`**, claim and workspace
  retained. The state covers a crash before the launch, a crash after it and before the
  upgrade, and an interrupted cleanup of a failed launch; they are indistinguishable, so
  treating the state as free MUST fail, and so MUST waiting on it forever.
- **The marker is written before the launch and removed only on confirmed absence.** Writing
  it after a successful start MUST fail the test, since that is the window the unknown state
  exists for.
- **The waiting orphan is visible in the process, not only in the tracker.** A warning names
  the issue and the workspace on the initial classification and on every probe that answers
  unconfirmed or errors, and stops once quiet is confirmed. B10 owns this: #79's per-tick
  warning is reached through a live handle and an event stream, and a recovered marker has
  neither. `ben status` (B11) may expose the same record later; B10 does not depend on it.
- **Boot identity only.** The run identified by a marker from a previous boot is gone by
  construction. Within one boot a stale group id can answer *alive* about a stranger — which
  costs a spurious wait, not safety, since a group persists while any member remains and a
  process's group cannot change under it. Per-process start time is deliberately not
  implemented.
- **The group-id seam is `harness.Launch`, not `core.RunHandle`** (which stays three methods —
  a group id is meaningless to a remote substrate, §13/#46). `core.RunnerOptions` carries it
  from the assembly. Once `cmd.Start` has succeeded, `Start` MUST return a handle: a sink
  failure is delivered as a terminal `failed` event with the group taken down through the
  ordinary ladder, never as a returned error. Returning an error there would leave a live
  group with no handle, nobody to stop it, and no marker upgrade — and today every
  `return nil, err` in `Start` precedes the process existing, so that equivalence is the thing
  under test.
- **Deterministic SIGKILL regression for the assignment→label window:** killed after
  `AddAssignees` and before `SetStateLabels`, restart adopts the claim and dispatches at
  `attempt 1` — the issue is neither abandoned nor read as published. Covers the kill landing
  before *and* after the read-back verification, since both leave the same tracker state.
- Mid-`done` kill (state labels cleared, publish comment not yet posted): restart finishes
  the projection from §9.7 evidence and does **not** spend an agent run re-deriving it.
- Mid-`failed` kill (label set, claim not yet released): restart completes the release and
  leaves the label standing.
- Label history says `done` but §9.7 finds no PR and no remote branch → `needs-review`,
  workspace kept. Silence is never the verdict.
- A stale PR from an earlier re-queued cycle does not make a freshly-killed claim read as
  published — the regression that rules publish-evidence-alone out as the classifier.
- **A re-queued `needs-review` issue is not read as `done`.** Re-queue retains the assignment
  and removes only the label, so it opens no new claim cycle and presents the same shape as a
  published issue — with a stale PR corroborating. The fixture MUST include that PR, and the
  verdict MUST be `backoff`, driven by the removed label's identity.
- **Candidate fetch is unfiltered:** an issue closed while the daemon was down, and one
  stripped of its required labels while down, are both still seen and released. A partition-
  or state-filtered fetch MUST fail this test.
- **An interrupted projection yields exactly one verdict.** Projection adds before removing,
  so `ben:running`+`ben:failed` and `ben:running`+`ben:needs-review` are both reachable;
  classification resolves each to the most recent standing `labeled` event and removes the
  residue. A set-based classifier matching two rows MUST fail.
- Assigned to the principal with **no standing `assigned` event** (event retention, transfer)
  → `needs-review` with the claim retained and a loud operator error. Never a guess.
- **All four milestone comments converge, not just the terminal two.** Killed between the
  label write and the comment, restart posts it — for `claimed` and `needs-review` as well as
  `failed` and `published`. Run recovery twice and there is still exactly one of each.
  Requires the §8.4 marker-based idempotency contract.
- **The gates owe comments too.** Gate 3 (no standing `assigned` event) and gate 2's
  unorderable fallback both park `ben:needs-review`, and both post its milestone comment. A
  rule scoped to the projection table only MUST fail this test.
- **The `claimed` comment does not repost on re-entry.** `ben:claimed` recurs within a cycle
  (§9.3 maps preparing, verifying, and backoff onto it), so a run that goes
  claimed → running → verifying → backoff → running leaves exactly one `claimed` comment.
  Keying that milestone on each transition rather than the cycle's first MUST fail this test.
- **Two daemons recovering one published issue do not both release.** Both hold assignments
  (one crashed after assigning, before its read-back released it); recovery runs §8.4
  arbitration, so exactly one releases and the issue never becomes unassigned-and-unlabelled,
  which is to say dispatchable. A gate that releases unconditionally MUST fail this test.
- **Partial publish evidence is not `done`.** A remote branch with no open PR, a PR closed
  unmerged, and a branch not descending from base each route to `needs-review`, not `done` and
  not an unmatched row. The split is §9.7-complete versus anything less.
- **A reconstructed `failed` comment is honest about its reason.** Same-host restart takes it
  from the transition log; fresh host states that it did not survive. Never invented, and the
  comment is never skipped.
- Restart with a `done`-awaiting-merge issue does not resurrect it, and **adopts a held-claim
  record** so §9.8 can release it later without another restart (decision 14). A recovery that
  leaves the issue alone with no record MUST fail this test.
- **A candidate open now but closed inside this claim cycle** — closed and reopened while the
  daemon was down — is released by **gate 1**, on the event, using the history step 2 already
  read. **The fixture MUST have the PR merged**, because that is precisely the case the
  projection table cannot serve: `FindPR` is open-only (§9.7), so a merged PR reads as
  *incomplete* publish evidence and the table would park `ben:needs-review` with the claim
  retained — on an issue whose reopen is the evidence against that verdict. An implementation
  that routes this through the table MUST fail the test. Startup and the §9.8 sweep must reach
  the same verdict from the same fact.
- Recovery never removes an assignment that is not this daemon's principal, including when a
  human co-assigned themselves while the daemon was down.

## B11 — CLI and daemon lifecycle

**Spec:** §10.3, §11 · **Depends on:** B01, B03, B08

`ben run` (graceful shutdown: stop dispatch → interrupt in-flight → wait for confirmed
termination → land what was already ordered; SPEC §9.8 as amended 2026-08-12 — shutdown
initiates no new release and no new terminal projection), `ben status [--json]` over the
white-box state files, `slog` JSON logging with
correlation attrs, state-dir layout (run records, transition log, transcripts, tokens),
single static binary build, sample systemd unit.

**Also owns the adapter kind registry** (assembly decision 13): `tracker.kind`/`agent.kind` →
registered kind, then `Structural` → `New` → `Ready` on `ben run`, and `Structural` alone in
`ben config effective`. It lands here rather than in B04/B06 because it is CLI wiring and
because there is no orchestrator to construct until B08.

The table itself and its `config effective` half landed early as `internal/registry` (#55):
deferring the registry had deferred nothing but the name, while the loader carried one list
and the CLI another, and `agent.provider` went structurally unvalidated. B11 owns the rest —
`New` → `Ready` on `ben run`.

**Repository seam** (#54): the workspace provider's remote and fetch credential come from the
tracker through `core.RepositorySource`, so assembly order is `Structural` → `New` → `Ready` →
`workspace.RepositoryFrom` → `workspace.New`. `Ready` is what resolves the credential and reads
the repository's own clone URL (§5.7, §5.8), so asking earlier is a refusal (`ErrNotReady`), as
is a tracker kind that cannot name a repository at all (`workspace.ErrNoRepositorySource`). The
wirer never parses `tracker.provider` (§5.2.5).

**Acceptance**
- SIGTERM during an active run: agent interrupted, claim handling per stop semantics, clean
  exit code.
- Shutdown stops dispatch and keeps the orchestrator and the context passed to
  `AgentRunner.Start` live while it waits for `Stop(StopInterrupt)` on every in-flight handle.
  It **initiates no new release and no new terminal projection** (SPEC §9.8 as amended
  2026-08-12): the claim and the `ben:*` label stand, and §9.10 resumes the work at the next
  start. Releases already ordered before the signal complete, and only after
  `TerminationConfirmed`. An unconfirmed result retains the claim and is re-probed; shutdown does
  not cancel that context or exit while the group remains unconfirmed (SPEC §7.5, §9.8, §11;
  #53). Bounding it instead requires a recovery-visible quarantine, which is a SPEC/B10 decision
  and not B11's to take unilaterally.
- **B11 is therefore not dogfoodable before B10 (#8).** Under the amended semantics an ordinary
  graceful restart leaves a claimed, labelled issue that only §9.10 recovery resumes, and until
  B10 lands nothing does. Merge order is unaffected — B10 depends on B08/B09, not on B11 — and
  the gate is recorded on #76.
- `ben status` works read-only against a live daemon's state dir.
- **Assembly test:** loader → kind selection → `Structural` → `New` → orchestrator
  construction, end to end. An unknown `tracker.kind`/`agent.kind` is a named refusal.
- The workspace provider is built from the tracker's `core.RepositorySource` answer; no
  component outside the adapter reads `tracker.provider` (#54).
- `ben config effective` reports a malformed provider block for an adapter that could never
  have been constructed — the case a method-shaped `Validate` structurally cannot serve.
- **`config effective` touches nothing external:** asserted with no credentials in the
  environment and no harness binary on `PATH`. It MUST NOT call `New` or `Ready`.
- `make workflow-check` validates the dogfood `WORKFLOW.md` against the **real** adapters in
  credential-free CI — the deferral B04 took because `parse` demanded a token.
- `ben run` refuses to start on a structural error, on a construction error, and on a
  readiness error, each distinguishable in the message.

## B12 — Integration harness and invariant suite

**Spec:** §12 · **Depends on:** all

The §12.3 invariant scenarios as end-to-end tests over the fakes; adapter fixture
tests against recorded harness/API streams; the RECOMMENDED real-integration smoke profile
(canary repo + real `claude-code`, manual/nightly) scripted and documented.

**Acceptance**
- All §12.3 scenarios green in CI without network or real subprocesses.
- Scenario 1 is parameterized over **every** write boundary of a multi-write projection, not
  just the state names: the assignment→label window, mid-`done`, and mid-`failed` each
  converge on restart with the issue correctly classified (§9.10, assembly decision 11).
  Boundaries *inside* one projection count — label-added-but-old-not-yet-removed, and
  labels-settled-but-milestone-comment-not-yet-posted — since add-before-remove makes both
  reachable and each has its own verdict.
- Scenario 2 asserts the revised §12.3-2 invariant in both branches: with an orderable log,
  exactly one daemon wins and the losers release only themselves; with an unorderable one,
  every claimant yields and none dispatches. No human assignment is removed in either.
- Smoke profile runs one issue end-to-end on a canary repo from a single documented command.

---

## Assembly-level decisions (made here, not in a wayfinder ticket)

Recorded so the build doesn't re-litigate them; each is small and reversible:

1. `ben run` as the daemon entrypoint name.
2. `workflow_key` = sanitized parent-dir basename + 8-hex FNV-1a of the absolute
   `WORKFLOW.md` path.
3. `run` template var finalized as `{id, previous_outcome}` — and extended to
   `{id, previous_outcome, previous_attempt}` by #61's §5.6 amendment.
4. Static retryable verdicts assigned per §7.3's table.
5. `created_at` added to the normalized issue model (FIFO dispatch needs it).
6. Verification/continuation composition: clean exit without publish evidence → continuation
   until `max_turns`, then `needs-review`; complete evidence → `done`.
7. Claim retained at `done` (blocks re-dispatch while the PR awaits review); claim released
   at `failed` (label blocks re-dispatch until a human clears it). **Amended 2026-08-07 (#15):**
   the original clause "distinguished from orphans by the absence of a state label" was
   wrong — that shape is equally a claim killed before label projection. Distinguished by
   label history instead; see decision 11. **Amended 2026-08-07 (#27):** retention had no stated
   end other than a restart; it is now bounded by a running daemon — see decision 14.
8. `dispatchable` additionally excludes any issue bearing a `ben:*` state label.
9. GitHub adapter rejects empty `required_labels` at validation.
10. Defaults: `max_concurrent_agents` 3, `max_turns` 4, `max_attempts` 3 (others follow the
    reference implementation's values; all tunable).
11. **Recovery classifies from positive evidence** (#15, human sign-off 2026-08-07;
    SPEC §9.10). Multi-write projections have no atomicity on GitHub, so every intermediate
    state is made self-describing rather than closed. Four things make that work, and each
    replaced a simpler rule that failed review:

    - The claim-establishing `assigned` event anchors the reading and defines the claim cycle.
    - Classification reads **ordered label events, not the label set**. Projection adds before
      removing, so an interrupted one leaves two `ben:*` labels standing and a set-based
      reading returns two contradictory verdicts.
    - **Which** label a transition removed is the discriminator, not merely that one was
      removed. Re-queueing a `needs-review` issue retains the assignment and removes only the
      label, so it opens no new cycle and mimics `done` exactly — with a stale PR to
      corroborate. `done` clears `ben:claimed`/`ben:running`; a re-queue clears
      `ben:needs-review`.
    - The candidate fetch is **unfiltered**. A claim needing cleanup can outlive the queue
      partition: closed or de-labeled while the daemon was down, it would be invisible to a
      queue-shaped fetch and never released.

    Publish evidence corroborates but never substitutes, and the `done` split is
    §9.7-**complete** versus anything less — partial evidence is a contradiction, not a pass.
    Absence of a fact is never evidence; an unaccountable claim fails closed rather than
    guessing. A contested candidate runs §8.4 arbitration rather than releasing
    unconditionally: two daemons recovering one published issue would otherwise both let go,
    putting finished work back in the queue.

    Adds two cache-bypassing recovery reads to the tracker contract (SPEC §8.2) and a
    marker-based idempotency contract on milestone comments keyed **per projection
    occurrence** (§8.4) — without the contract an interrupted terminal projection leaves a
    comment that can never be written; keyed by claim cycle instead, a re-queued issue's
    second `needs-review` comment would be suppressed.
12. **Contested claims resolve from the ordered assignment log** (#15; SPEC §8.4). Durable
    evidence decides; ambiguity yields safely. First standing assignment wins, ordered by
    `(created_at, event id)` because GitHub timestamps are second-granularity and racers tie
    routinely. Withdrawn logins are skipped; unorderable ones make everyone yield. Losers
    release only their own assignment.

    Deliberately **no cooldown or deferral** on the yield path. A rank-based de-synchronizer
    was proposed and rejected: it only engages once the evidence protocol has already failed,
    it cannot establish ownership when it does, and it would put scheduling state inside the
    tracker adapter or change the claim contract. Plain release is fail-closed, and §10.1
    scopes multi-daemon as tolerated-but-wasteful with no queue SLO. Revisit only if progress
    against a degraded event-history endpoint becomes a v1 requirement. Claim labels stay
    deferred to §13: they would delete the ambiguous window but leave the race untouched.
13. **Adapter kinds register at package level; `Structural` + `New` before construction,
    `Ready(ctx)` after** (#17, human sign-off 2026-08-07; SPEC §5.7, §5.8, §7.1, §8.2, §11).
    Both adapters had already grown this split internally — `parse`/`ParseProvider` versus
    `checkRuntime` — so the contract now exposes it rather than hiding it behind a single
    `Validate(provider)` that meant structural-only on the tracker and structural-plus-
    subprocess on the runner.

    Validation cannot be a method on the constructed adapter: a malformed config fails during
    construction, leaving no instance to ask. `Structural` is a property of the *kind*, it is
    pure (no network, filesystem, or subprocess), and it takes the whole adapter config —
    opaque block *and* core-owned fields — because rules like non-empty `required_labels`
    span both, and validating a new block against a previous reload's core fields would be a
    silent hot-reload bug.

    Binding is only as good as its enforcement: the adapter owns the whole child environment,
    and `BEN_` is reserved to the orchestrator **from both sides** — `RunSpec.Env` may carry
    nothing else (`failed(launch_error)`), and a provider block may not define the prefix in
    any environment surface (a `Structural` refusal at load, since it is a property of the
    file). A narrower rule — no overriding *provider-derived* keys — leaks twice: an omitted
    provider field derives nothing, so the credential can be freely injected, and `HOME`
    arrives via the allowlisted passthrough yet redirects keychain lookup. A blacklist would
    have to enumerate every variable any harness might read; the namespace makes collision
    inexpressible instead, but only while it is exclusive.

    **Credential policy:** an explicit `$VAR` resolving empty is rejected by the **loader**
    (§5.5) and never reaches adapter validation at all; an *omitted* field with a documented
    env fallback is structurally valid and unresolved until `Ready`. That is what
    lets `make workflow-check` validate the dogfood `WORKFLOW.md` against the real adapter in
    credential-free CI, which B04 could not do and deliberately deferred here.
14. **A running daemon releases retained `done` claims, via a held-claim sweep** (#27, human
    sign-off 2026-08-07; SPEC §8.2, §8.3, §9.2, §9.8, §9.10). Retention at `done` (decision 7)
    named no end but a restart, and §9.8 reconciled only running and parked issues — so on the
    canonical one-long-lived-daemon topology (§10.1) the claim outlived the merge indefinitely,
    and a merge-then-reopen left an issue nobody could dispatch, since the claim excludes *us*
    too (decision 8 and the §8.3 clarification: any assignee blocks, not just another party's).

    `done` now leaves a **held-claim record** — no workspace, no runner — and each tick refresh
    of the held set is **one** ETag-conditional `HeldClaims` read. Release on a `closed` event
    in the claim cycle, or on loss of the required labels; confirm an absence with a `Get`
    before dropping anything.

    **Where that event is read from is part of the decision.** A still-closed issue is settled
    by the list response. A close a reopen has undone survives only in the log, so the sweep
    reads `ClaimHistory` when a held record's **revision** moves — the revision triggers, the
    event decides. Without that trigger the rule eats itself: history read only for records the
    list shows closed can never see a reopen, the case the two event kinds were added for.
    At startup the same rule lives in §9.10 **gate 1**, where step 2's history read has already
    paid for it, and it MUST NOT be left to the projection table: `FindPR` is open-only, so a
    merged PR reads as incomplete evidence and the table would park the reopened issue
    `ben:needs-review` with the claim retained.

    **The trigger is an opaque `revision`, not `updated_at`** (SPEC §8.3). GitHub timestamps are
    second-granularity — the same fact §8.4 refuses to trust for ordering — so a close and a
    reopen inside one second leave `updated_at` unmoved, the sweep never looks at the log, and
    the claim is held until a restart: the original bug with a smaller window rather than a fix.
    **The MUST is scoped to a named projection, and the projection is exhaustive.** The token
    derives from §8.3's **revision projection** — `state`, the tracker's reason for the most
    recent state change (GitHub `state_reason`), and `updated_at` — **exactly**: no fewer and no
    more. Both bounds are load-bearing in opposite directions. No fewer is the bug above. No
    more would spend a change-log read on every rename, in the one place per-issue reads were
    ruled out; and a rule forbidding only the *whole* payload is not the same rule, since
    folding in one extra field satisfies it while costing the same reads. Narrow is *sufficient*
    because the token gates one rule only — §9.8's history read — while terminal state,
    partition membership, and ownership are each read straight from the same list response.
    A contract written over the whole representation, as the first draft was, promises movement
    the implementation does not deliver and cannot afford to.

    **Enforced structurally, because "exactly three" cannot be enforced by counting negatives.**
    The projection is a closed struct, one function maps the payload onto it, and the hash sees
    only the struct — so widening requires editing a named type the compiler routes everyone
    through, and a test pinning that type's fields catches every possible field at once. Three
    successive review rounds each found the previous guard too weak by one field (whole-payload
    rule → title; seven-field table → milestone), which is the argument for making the illegal
    state unrepresentable rather than enumerating it — the same move decision 13 makes with the
    `BEN_` namespace.

    **Residual accepted, not engineered around:** a close-and-reopen the projection cannot
    express — on GitHub, an already-`reopened` issue closed and reopened again inside one
    second, leaving state, reason, and timestamp where they were. A conditional poll cannot see
    it at all in that case (304). Gate 1 reads the log unconditionally and catches it next
    start; webhooks (§13) are the seam if the latency ever matters. Same posture as decision
    12's unorderable-log residual.

    Three things were decided against, each for a stated reason:

    - **A `Get` per held claim** (the obvious reading of "keep `done` tracked"): O(held) per
      tick, and held grows with human review latency. At the default 30 s interval that is 120
      requests per hour per claim against a PAT's 5,000, so a few tens of open PRs exhaust the
      budget. The list read is O(pages) and unbilled while nothing changes.
    - **A second, slower ticker** (the issue's option 2): bounds the same O(held) cost by
      cadence and needs a cadence constant — a knob in what §5.2.7 calls a closed set — for a
      worse cost curve than one conditional read.
    - **Keying release on current tracker state**: a close-and-reopen inside one interval reads
      `open`, so the release window is skipped exactly as it was between two startups. Hence the
      two new `ClaimEvent` kinds (`closed`, `reopened`): the event stands in the log after the
      reopen, so evidence outlives the moment. This is the same argument decision 11 makes about
      label events versus the label set, applied to issue state.

    Records are a cache, not state: recovery rebuilds the held set from `ClaimedByPrincipal` and
    the §9.10 table, so the `done` verdicts there adopt a record rather than leaving the issue
    alone. **Documented gap, not fixed:** a PR closed unmerged neither closes the issue nor
    leaves an event on it, so no sweep can see it; §9.10 parks it `needs-review` at the next
    start, and §13 names the PR-state sweep and its trigger.
15. **Safety-asymmetric enums fail closed at zero.** When one enum member authorizes a
    safety-sensitive or irreversible action, the zero value is a conservative, non-authorizing
    member; authorization requires a named, nonzero value. Both verification verdict types
    therefore default to `VerdictUnknown`, never `VerdictPublished` (#37, #45/#7), and
    `core.Termination` defaults to `TerminationUnconfirmed`, never `TerminationConfirmed`
    (#53/#78). Fixture constructors that need the authorizing value state it explicitly, so
    omission follows the conservative path. Both fixes arose from the same failure mode: an
    unfilled field read as permission.
16. **Parked records ride decision 14's sweep read** (#36; SPEC §8.2, §9.8). Decision 14 removed
    the O(held) poll and left the identical shape next door: §9.8 refreshed `needs-review`
    records with a `Get` each, and that set grows with review latency for the same reason the
    held set does. The read decision 14 already makes answers both parked rules — it is
    unfiltered in state and labels, so a parked issue is in the response with the `state` the
    terminal rule reads and the labels the unpark rule reads — so parked records are swept
    beside held claims, and only the **running** records keep a per-issue `Get`.

    **The bound is on round trips, not only on billed requests**, and decision 14's framing
    ("304s are unbilled, so the steady state is free") hid two costs that made the parked case
    worse than the arithmetic suggested. The per-issue refreshes are issued *serially in one
    pass*, so a review backlog delayed the reconciliation of every running record queued behind
    it; and an authenticated 304 refunds GitHub's core allowance but **not** its request-point
    allowance (§8.5, `budget.go`), so O(parked) spent a real allowance every tick to learn
    nothing.

    **Absence keeps decision 14's rule and gains a verdict**, and the confirming `Get` is
    **capped at one per tick**. A parked record missing from the read is still confirmed and never
    acted on; a confirmation that says *the principal is not assigned* drops the record without a
    release, since the assignment **is** the claim (§8.3), leaving the state label standing.
    §9.10 reaches the same place from `ClaimedByPrincipal`, which no longer returns the issue.

    Two things the first implementation got wrong here, both found in review of #143 and both
    worth stating because the shape recurs. **A per-record confirmation is still O(parked)**: the
    case that produces one absence produces all of them — a human unassigning a backlog — so the
    fix has to bound the confirmations per tick, not merely move them off the hot path. And
    **the confirmation answers two independent questions**: whether the claim is ours decides
    whether a release is owed, whether the issue is *terminal* decides the workspace. Checking
    assignment first made a closed-and-unassigned issue — one human gesture — keep a worktree
    §9.8 disposes.

    **The unpark rule reads the response's labels only when BEN's own projection has landed.**
    Until then the tracker is still reporting the labels from before the park, and the rule reads
    an unlanded write of ours as a human's gesture. That was reachable on `main` through §9.5's
    `queued → needs-review` drift park, which projects the first `ben:*` label this issue ever
    gets: the record re-queued itself on the next tick. **Both bounds on the condition are
    load-bearing, in opposite directions** — the third review finding was one of them:

    - **The projection, not every owed write.** `owesAnything()` covers milestone comments and
      local effects, and a comment can fail indefinitely on its own §8.5 allowance, so gating on
      the queue let one wedged comment suppress every human re-queue for the life of the record.
      The mark is a field on the owed effect, set by the one call site that writes a `ben:*`
      label, and a source-level test pins that there is exactly one such site and that it is
      owed through `oweProjection` — the label rule's own tests are all driven by the mark, so
      none of them can see it omitted.
    - **This rule, not the classification.** Gating the whole classification instead lets a
      permanently failing label write hide a deleted issue forever, which is a test this repo
      already had.
17. **Held-claim absence confirmations share the sweep's bound** (#148; SPEC §9.8). Decision
    14 removed a `Get` per held claim from the ordinary poll but left that same shape on its
    absence path: K claims missing from one assignee-filtered response launched K concurrent
    confirming reads on the tick that noticed. The case that produces one absence commonly
    produces the whole set — a human unassigning the principal across a review backlog — so
    moving the reads off the list path did not bound them.

    The held set now spends **at most one** absence-confirming `Get` per tick, separately from
    the parked set's budget. Candidates are sorted and offered in an explicit rotation whose
    cursor advances on the offer, not its outcome, so a confirmation that keeps failing cannot
    retake the only slot and starve the others. K simultaneous absences therefore resolve over K
    ticks. That latency is accepted: an absent issue is either no longer ours or no longer there,
    while the alternative makes one human gesture cost O(held) concurrent requests.

    The evidence rule and every verdict are unchanged. Absence itself decides nothing; the
    confirming `Get` still decides deleted, no longer assigned, or a lagging list response.
    `ClaimHistory` remains one read per observed revision change, and settled releases remain
    owed writes. Neither cost is folded into this confirmation budget: changing either is a
    separate policy decision.
18. **One claim-scoped base/target branch applies on both substrates** (#152; SPEC §5.2.4,
    §5.6, §6.2, §9.7; accepted ticketprep packet
    `sha256:772099818aff9bd25682abe88cbc6852c8cb828b650c91fc4019b7dd7b6974be`).
    `workspace.base_branch` is one unqualified branch selector, not separate base and publish
    settings. Omission resolves the repository default when a new assignment epoch first
    prepares. The selected target is stored atomically with that epoch's verification base and
    retained across retry, rollback, reload, restart, and default movement. Prompt
    `target_branch` is trusted guidance; exact PR-base equality is authoritative evidence.

    Local `FindPR` and the remote fact source share one cardinality rule: zero exact-head open
    PRs is incomplete, one supplies its full facts, and a second is `core.ErrPRAmbiguous`
    regardless of update order or target. The Airlock mirror, not workflow state or sandbox
    state, selects and stores each remote claim's target; remote verification reads that claim
    record. A targetless pre-amendment local, mirror, or remote-cycle record authorizes no
    same-epoch prepare, restore, prompt, verification, hook, or launch. After the §9.10
    empty-principal deployment drain, only a later assignment epoch may preserve valid outgoing
    base/cycle facts and replace the record with a complete tuple.
