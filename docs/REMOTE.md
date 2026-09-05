# The v2 execution substrate — seams without v1 configuration

BEN v1 runs an attempt as a subprocess in a git worktree on the daemon's own host. This document
describes the **v2 seams** that let an attempt run somewhere else instead — a remote workspace and a
durable foreign process — and, just as importantly, what does *not* change when it does.

Nothing here is reachable from a v1 workflow or the daemon's production assembly. #192 exposes an
explicit construction seam exercised directly by tests; it deliberately adds no workflow key,
registry entry, or `ben config effective` field. Configuration, credentials, the client, and
assembly belong to [#194](https://github.com/srhg-ai-7cef3f93/ben/issues/194). This is the boundary
that adapter lands against ([#192](https://github.com/srhg-ai-7cef3f93/ben/issues/192), against
[#46](https://github.com/srhg-ai-7cef3f93/ben/issues/46)).

[SPEC.md](../SPEC.md) is locked and describes v1. Nothing in this document amends it.

## What stays with BEN

The division is the whole design, so it is worth stating before the interfaces.

BEN keeps everything it already owns: claims and the claim epoch, retries and backoff, request
budgets, prompt rendering, the Claude/Codex argv, the raw-stream translation, the `ben:*` state
projection (SPEC §9.3), and the publish-evidence verification behind `done` (SPEC §9.7).

A backend supplies two opaque things and nothing else:

- a **workspace/sandbox reference** — `remote.Identity`, carrying the claim cycle, the issue branch,
  the workspace-cycle base SHA, the backend's sandbox id, and an immutable profile revision;
- a **foreign process** whose bytes, events and status are opaque — `remote.ProcessBackend`.

SPEC §3 invariant 6 holds by construction: a `remote.Envelope` carries an arbitrary raw byte
chunk. `internal/remote` frames stdout chunks into lines — a chunk may split a line or carry
several — but never parses provider content. The `remote.Translator` that parses each complete line
into `core.Event` values is the *provider adapter's* own function, the same
`translate(line []byte) []core.Event` claude-code and codex-exec already have at their boundary.
The unfinished decoder tail is part of the durable checkpoint, so a restart cannot frame the same
bytes differently.

The composition sits behind the seams that exist. `remote.Attempt` is a `core.RunHandle` and
`remote.Runner` is a `core.AgentRunner`; there is no second control loop. Assembly also supplies
a `remote.DurableConsumer`. Its acknowledgement, not a successful send into the receive-only
`RunHandle.Events` channel, is the durable handoff. The channel is a live notification of work
already accepted by BEN's durable inbox.

## The rules

**A consumer disconnect is not a remote cancel.** `Events` returning because a context ended says
the reader stopped and nothing at all about the run. It neither signals the run nor invents a remote
`failed(killed)` outcome. Only mode-preserving `Stop` and workspace `Delete` act.
The claim is retained until the backend supplies positive lifecycle evidence.

**A read that failed is not a fact about the run.** The backend's event log is cursor-addressed and
retained, so a transport error from `Events` means BEN could not reach a process that is otherwise
untouched — the same read, from the same admitted cursor, is the one that succeeds when the
connection comes back. `remote.ReconnectPolicy` bounds the *waiting*: doubling from `Initial` to
`Max`, with `Budget` the accumulated backoff past which the reconnect stops being a blip and is
logged as an outage. It never bounds the attempt. Past the budget the attempt is **held** — stream
open, cursor unmoved, nothing committed — and the orchestrator goes on retaining the claim until the
run's own termination can be observed (SPEC §9.8). Only a backend that *answered* ends a stream:
a sealed log with no terminal event, a discontinuity BEN can neither measure nor resume from, or a
durable envelope the backend adapter cannot decode. The last is not a transport failure: the log is
append-only, so asking for that cursor again can only return the same unusable evidence.
The log line is the whole of what a held attempt emits, so it is written once per failed read
rather than once per outage; the daemon passes its own logger through `RunnerConfig` (#275).

**Interrupt delivery is not termination.** A backend may move a TERM request to
`PhaseSignaled`, which is not quiet and never authorizes touching the workspace — the same
distinction `core.Termination` draws between teardown being requested and the execution domain
being positively quiet (SPEC §7.5, §9.8).

**Stream sealed, process reaped, and domain quiet are three facts.** Closing `Events` follows the
stream. Closing `RunHandle.Done` follows only direct-process reaping. Workspace reuse follows only
an explicit `DomainStateQuiet` observation from a reachable backend. None is inferred from either
of the others or from the diagnostic `Phase`. `remote.MayReuse` is the single workspace predicate;
`remote.Reacquire` and `remote.Dispose` are the two operations gated on it.

**Stop modes remain different.** Interrupt asks the backend for the patient TERM/grace/KILL ladder
and keeps streaming when termination is unconfirmed, so an agent winding up can still report its
outcome. Discard requests immediate termination and abandons live delivery. Both return only fresh
backend evidence.

## Recovery

`remote.Journal` owns two durable orderings, and they point in opposite directions on purpose.

Workspace-cycle replacement has a third ordering. A new standing approval is a
new sandbox address; before `internal/remotews` publishes that replacement, it
writes a separate ended-cycle disposal record under the old address. The record
contains the old approval-cycle identity and assignment pin and survives deletion
or further replacement of the live issue record. Recovery enumerates these
records before it classifies tracker claims, and an unreadable enumeration blocks
dispatch and tracker-claim release rather than treating an unknown owner as
absent.

The replacement's claim pin is recorded while retaining every old pin named by
an ended-cycle record. Old and live cycles then proceed independently: the live
cycle may prepare or finish while an old backend delete waits, and one issue may
owe several old addresses. After a delete's compute, volume, and tombstone fields
all confirm, BEN durably records that answer before discarding the old pin and
unlinking the obligation. A restart in either cleanup step repeats only local
cleanup. Current-cycle read/modify/write operations remain serialized per
workspace key, so a concurrent disposal cannot lose or overwrite a replacement.

Request-digest JSON is a versioned durable wire format, not the current Go
struct layout. Legacy process and script-hook requests retain their original v1
preimage when the new Git/direct-command fields are empty; a request using
either extension has an explicit v2 preimage. This preserves old journals
across ordinary upgrades and preserves new legacy-shaped journals across a
rollback. A binary that does not understand v2 cannot safely reinterpret an
active scoped run. Upgrading from, or rolling back to, a binary without v2
support therefore has an [empty-claim precondition](DEPLOY.md#upgrading-into-claim-scoped-bases-and-targets):
stop the old daemon, prove its claim principal owns no running, parked, or held
issue, and prove the review-run store has no retained run before starting the
other binary.

**Identity before the act.** `ProcessRef` contains the complete workspace identity (including
branch and trusted base), BEN's run id, and a canonical digest of the immutable `ProcessSpec`.
Every backend method takes that same sandbox-scoped reference. The reference and dispatch mark are
on disk before `Start` is attempted. Same address/different request is `ErrProcessMismatch`, never
an attach.

A `Start` error is ambiguous. The backend-generated run id exists only in the response, so a lost
response cannot be resolved by `Attach`. `Runner.Start` first replays the exact `Start(ref, spec)`
request: Airlock returns the stored idempotent result without creating a second run. If both
responses are unavailable, the journal retains a non-secret projection of the orchestrator's
`RunSpec`. A later daemon recomposes the provider invocation from its current binding and compares
the complete request digest before it sends the exact replay; provider environment and credentials
are never persisted. Airlock can resolve the committed/live case sooner from the sandbox's single
active-run slot, but only when the run's `ben.process` label matches the one-way identity of the
exact `ProcessRef`. A null slot is only a snapshot, so the pre-tracker startup survey returns
`ErrProcessUnresolved` without replaying. Recovery then passes §9.10's tracker and pinned-epoch
gates and reads §9.7: published work and verifier errors never replay, while only an incomplete or
contradicted active projection routed to orphan backoff may continue. Its standing required-label
event must match the persisted workspace cycle and a fresh tracker read immediately before the
backend is touched, so remove-and-reapply cannot restart the superseded cycle even when the
assignment stayed put. Revoked, terminal, completed and superseded-approval work is never
restarted. This follows Airlock's frozen
[identity and idempotency contract](https://github.com/srhg-ai-7cef3f93/airlock/blob/main/docs/v2/01-identity-and-idempotency.md).
Once a successful response has supplied a backend run id, the journal retains it and restart
attachment requires that id. Creation-key replay is not used as permanent naming: Airlock's
idempotency record has a bounded lifetime, while the run id is the durable resource handle.

The process seam has four absence-shaped answers, and only two are absences. `ErrNoProcess`
means `Start` never crossed the backend's durable acceptance fence, so retiring BEN's dispatch
journal is safe. `ErrProcessRefused` is the narrower statement (#284): the request crossed to the
backend and the backend refused the body before committing anything — a payload over a profile
limit, a rejected environment key, a malformed request. It is carried as `remote.ProcessRefusal`
with the backend's code, sanitized message and exceeded limit, and it satisfies `ErrNoProcess` too,
because nothing exists and every consumer that retires a dispatch on a confirmed absence is right
to. What it adds is that the same request would be refused again: `Runner.Start` hands it to the
orchestrator as a launch that never happened rather than an ambiguity-preserving handle, and a
backend answers an unchanged body from its own record without a request. `ErrProcessUnresolved`
means that fence exists but no permanent run id was learned; the keyed request may have committed,
and neither exact replay nor the backend's authoritative resource model has resolved it yet.
`ErrProcessUnavailable` means a permanent run id was learned but the accepted resource is no longer
readable — including a not-found response that may be a tombstone or cross-tenant hiding. The last
two are unconfirmed lifecycle states: they retain the journal, the claim, and the workspace, and
never authorize a replacement or disposal.

**The act before the position.** For each envelope, `DurableConsumer.Commit` first accepts the raw
transcript bytes, normalized events, and full decoder checkpoint under an idempotency key. Only
after that acknowledgement does the local attach journal advance its cursor. A crash between those
writes replays the same key; the consumer reports it already accepted, and the journal catches up.
On attach, `DurableConsumer.Recover` returns those accepted consumptions before the backend stream
resumes. That recovery is the delivery path for an outcome accepted just before the daemon died;
a terminal checkpoint never closes a new handle without first re-projecting its terminal event.
Cursor, decoder tail, and terminal-outcome bit move together. The consumer also makes terminal
acceptance sticky across distinct keys: later raw transcript bytes may be retained, but no second
normalized outcome can be accepted if the local journal died before mirroring the terminal bit.

The recovered raw envelopes also rebuild the sequencer's digest for every committed sequence. A
replayed sequence with the same payload is dropped; a different payload at any sequence at or below
the cursor is `ErrEventConflict`. A sequence beyond the next expected one, or a durable envelope the
backend adapter cannot decode, is `ErrEventGap`, because advancing across missing or unusable
evidence commits BEN to an event it never saw. Batch admission is atomic: finding a conflict or gap
leaves the admitted mark and digest set unchanged.

The one exception is a discontinuity the backend *measures*: a `RetentionGap` naming the cursor it
is about and the oldest sequence still retained. That pair is a range, so BEN commits the range and
one `failed(crashed)` outcome in a single durable act, drops the decoder tail, and resumes at the
retention floor — the range then travels with the recovered history, which is what lets a restart
tell an accepted expiry from a corrupt log. It is still an `ErrEventGap` to every reader that has
not learned to accept one. Draining continues after the acceptance; translation does not, so the
retained bytes past the gap are transcript rather than a second outcome. See
[AIRLOCK.md](AIRLOCK.md).

That advance is available before a terminal outcome, when the same durable act creates
`failed(crashed)`, and after that failure while the remaining transcript drains. It is not
available after `succeeded`: SPEC §7.4 makes the provider's terminal event ground truth, and BEN
cannot both preserve it and attach the failure that authorizes a gap advance. A later expiry is
therefore refused without moving the cursor.

`ProcessSpec.Limits` carries both stall and attempt timeouts to the backend. Enforcement belongs
there so both clocks continue while BEN is disconnected; reconnecting must not reset either.

## Concurrency

`limits.max_concurrent_agents` counts a different thing on each substrate, and both readings are the
same rule applied to the scarce resource.

On v1 it is local agent processes, and a `verifying` record stops counting once its execution
domain is confirmed quiet — the §9.7 check reads git and the tracker with nothing executing.

On v2 the scarce resource is the backend's allocation, so `remote.LeaseState` counts **active
backend runs and leases**. A held lease costs even with no run in it — a suspended workspace is
still a reservation — and a dispatched run costs until its termination is *confirmed*, which is v1's
exclusion read honestly on a substrate where the proof is domain quiet rather than `ESRCH`.

## Hooks

The four §5.2.6 lifecycle hooks run through the backend (`remote.HookExec`) and keep the workflow's
contract: the same phases in the same order, the same shared timeout, and the same per-phase
containment — `after_create` and `before_run` abort what they gate, `after_run` and `before_remove`
are logged and ignored.

Each firing has a BEN-chosen `HookID`, attempt number, canonical request digest, and its own durable
record. The dispatch mark lands before `StartScript`; a lost response is resolved by replaying that
exact idempotent request, and terminal plus domain-quiet evidence lands before the caller may
proceed. Restarting a
`before_run` therefore resolves the same mutation instead of executing it again. An unavailable
replay remains an aborting hook failure, so the agent cannot start while a prior hook may still be
changing the workspace.

One thing changes, in the safe direction. Locally a hook's environment is `core.EnvAllowlist` and
nothing else, because the daemon's host holds secrets a repo-authored script must not reach. A
backend sandbox holds none of them, so `internal/remote` composes no environment at all and the
profile defines what a script sees.

BEN-owned Git prepare and publish commands reuse that durable executor without
becoming configurable lifecycle hooks. They use direct argv, not a shell, and
carry a typed Git scope that the Airlock adapter translates to control-plane run
labels. Prepare materializes the exact daemon-observed commit. Coding receives a
different run identity and may use ordinary local Git only. Publish starts only
after coding is terminal and domain-quiet, invokes the credential-free
`airlock-git` packager, and reuses one operation key on replay. Review is another
distinct run and is likewise separated by the quiet gate.

## Conformance

The AgentRunner contract is now two suites, and the split is what lets a remote adapter prove the
outcomes without pretending to expose facts it does not have.

`internal/agent/runnertest` is the **universal** subset: asserted through `core.RunHandle` alone,
run unmodified by both local adapters, the local fake (`internal/fake`), and the remote fake
(`internal/remote/remotetest`). No case in it may name a pid, a process group, a working directory,
argv, stdin or an environment variable — its own source is scanned for those, so the separation
fails loudly rather than eroding.

`internal/agent/agenttest` remains the **local** suite. Every case in it drives a real child
process, and `scope_test.go` names the ones that are local *by subject*: what they assert is a fact
a POSIX process on this host has and a foreign process does not.

Two v1 behaviours are deliberately absent from the universal subset, and their absence is the
interesting part of the boundary rather than an omission. Liveness windows are the runner's
obligation on both substrates but by different mechanisms, so each is asserted where it is
implemented. Context cancellation kills a local child, while remotely it only disconnects a reader
and produces no invented terminal event; a universal assertion over it would have to make one of
the two substrates lie.

## Construction boundary

#192 stops at `remote.New(Options{...})`, `WorkspaceBackend`, `ProcessBackend`, `HookExec`, and their
durable stores. Tests select that bundle directly, and so does every backend: nothing in this
package knows what is behind them.

PR [#203](https://github.com/srhg-ai-7cef3f93/ben/pull/203) adds the first backend foundation,
`internal/airlock`, together with the `substrate:` section that names it — see
[AIRLOCK.md](AIRLOCK.md). What that changes here is only the sentence above about configuration:
writing `substrate:` is now an opt-in rather than an unknown-field refusal, and an omitted section
resolves to the local substrate, so every v1 workflow keeps working unchanged.

[#205](https://github.com/srhg-ai-7cef3f93/ben/issues/205) then adds the consumer these seams were
written for: `internal/remotews`, the SPEC §6.1 workspace strategy over `WorkspaceBackend`, and the
`cmd/ben` routing that selects it. It is the *sibling* of `internal/workspace` rather than a layer
over it — both answer the seam the orchestrator declares, and neither imports the other's lifecycle.
That completes [#194](https://github.com/srhg-ai-7cef3f93/ben/issues/194): a claim now reaches a v2
substrate end to end.

[#152](https://github.com/srhg-ai-7cef3f93/ben/issues/152) makes the configured base branch one
local-and-Airlock contract. The daemon-side mirror resolves the explicit branch, or the then-current
repository default, when it records a new assignment epoch and atomically stores that target beside
the epoch's verification base. `internal/remotews` retains the returned target in its cycle for
restoration, trusted prompt rendering, and the typed Git scopes for coding, publication, and remote
review; it does not become verification authority. The remote checker reads the target back through
the mirror's claim record, so two claims spanning a default movement may correctly require different
PR bases under one workflow configuration.

What the Airlock backend does **not** change is the substrate boundary above. `internal/airlock`
implements the three interfaces and adds two durable facts of its own that this boundary
deliberately does not model — which sandbox a claim acquired, and which backend run a dispatch
resolved to. It does not widen a seam, does not put a wire type in front of the orchestrator, and
does not add a second control loop.

## Not in scope here

No HTTP client, no credentials or endpoint configuration, no Kubernetes, no remote git verification,
no provider proxy, and no `SPEC.md` edit. The first four belong to a backend — `internal/airlock`
is where they live now — and remote publication verification is
[#193](https://github.com/srhg-ai-7cef3f93/ben/issues/193)'s, read daemon-side from git facts no run
can reach.

The SPEC §6.1 **workspace strategy** over `WorkspaceBackend` was the last of these, and it is no
longer open: [#205](https://github.com/srhg-ai-7cef3f93/ben/issues/205) landed it as
`internal/remotews`, with the daemon routing around it in `cmd/ben`. `ben run` now validates,
probes, reconciles and dispatches ([AIRLOCK.md](AIRLOCK.md) states the operator-facing half).

Remote publication verification stays where it was — daemon-side, from git facts no run can reach.
The trusted Airlock publish phase performs the side effect, but its success is still never BEN's
evidence that a branch or pull request exists, and sandbox state never supplies the required PR
target.
