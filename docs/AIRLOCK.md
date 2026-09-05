# The Airlock v2 execution foundation

BEN v1 runs an attempt as a subprocess in a git worktree on the daemon's own
host. This document is the operator-facing half of running it somewhere else
instead: the `substrate:` section of `WORKFLOW.md`, what BEN persists, what it
refuses, and what is not wired up yet.

[docs/REMOTE.md](REMOTE.md) is the contributor-facing half — the #192 seams this
backend lands against. [SPEC.md](../SPEC.md) is locked and describes v1; nothing
here amends it.

## Status: dispatchable

Read this first, because it decides whether the rest is useful to you today.

`substrate.kind: airlock` **loads, validates, renders and dispatches**. `ben
config effective` prints the whole declaration with no credentials and no
network, and `ben run` goes further: it constructs the backend, mints the
credential, reaches the endpoint, and checks that your tenant may still
provision the named profile. Those are the refusals you actually need while
staging a configuration.

`ben run` then **surveys every retained remote claim** and starts. A claim on
this substrate acquires or reattaches its sandbox, runs the agent's own command
through Airlock's durable process API, and is verified daemon-side — no local
worktree is prepared and no agent process is launched on this host.

[#205](https://github.com/srhg-ai-7cef3f93/ben/issues/205) landed the SPEC §6.1
*workspace strategy* (`internal/remotews`) and the daemon routing that completed
[#194](https://github.com/srhg-ai-7cef3f93/ben/issues/194). Two properties of
that path are worth stating here, because they are what an operator is trusting:

- **A local worktree is never prepared for a remote claim**, so the "two trees,
  one claim" hazard the previous refusal existed to prevent is not reachable —
  the remote strategy and the local one are alternatives, never both.
- **An Airlock coding process succeeding is not a publication.** Once that run
  is terminal and its process domain is confirmed quiet, BEN starts a distinct
  trusted publish run. That run invokes the credential-free `airlock-git`
  packager; Airlock derives repository, branch, base, operation, and run identity
  from control-plane-owned scope and its broker validates the submitted graph.
  The final §9.7 verdict is still read from the daemon-side evidence store
  ([#193](https://github.com/srhg-ai-7cef3f93/ben/issues/193)) — the canonical
  branch head, claim-time base/target pin, and the pull request — never from
  anything the sandbox reports about itself. A clean run that published
  nothing parks for a human; a PR against another target is contradictory.

Startup reconciliation is completed **before** ordinary dispatch resumes, and a
retained claim nothing can act on — a sandbox that is gone, owned by somebody
else, or pinned to a profile revision that has moved — refuses the daemon rather
than being dispatched around.

Set `substrate.kind: local` — or omit the section entirely — to run a workflow on
this host. Every workflow that predates this section keeps working unchanged.

## The declaration

```yaml
credential_sources:
  airlock:
    kind: projected_oidc
    issuer: https://oidc.eks.example/id/cluster
    audience: airlock-api
    tenant_claim: sub
    tenant_id: system:serviceaccount:ben:ben
    subject: system:serviceaccount:ben:ben
    token_path: /var/run/secrets/airlock/token
    min_ttl_ms: 300000

substrate:
  kind: airlock                  # or `local` (the default)
  airlock:
    base_url: https://airlock.internal
    profile: ben-agent
    auth_source: airlock

    # Optional — every value below is what an omitted key resolves to.
    tls_ca_file: ""              # a PEM bundle; empty uses the host's roots
    request_timeout_ms: 30000
    poll_timeout_ms: 70000
    poll_wait_ms: 30000
    settle_timeout_ms: 300000
    max_retries: 4
    idle_suspend_ms: 0           # 0 leaves the profile's own window
    delete_after_idle_ms: 0      # 0 leaves the profile's own window
    on_success: suspend
    on_failure: retain
    on_revoked: suspend
    on_shutdown: suspend
```

The top-level `workspace.base_branch` setting applies identically to local and Airlock workflows.
The daemon-side mirror resolves it (or the repository default when omitted), checks it during
readiness, and records the selected target atomically with each new assignment epoch's verification
base. A retained claim keeps that target across retries, restarts, and later default movement.
The configured branch may not be `ben` or begin with `ben/`: that namespace belongs to BEN's
per-issue branches, and using it as a target would collide with their Git refs or identities.

**The section is closed, not opaque.** `tracker.provider` and `agent.provider`
are exempt from strict key validation because an adapter owns their schema.
Nothing here is like that: every key is one BEN validates, and a typo in an
endpoint or a retention policy is not something to discover at the first
dispatched claim. An unknown key refuses at load.

**Nothing here is `$VAR`-resolved.** A base URL is not a secret — it is refused
outright if it embeds one — and a profile, a policy and a timeout are not places
a secret belongs. The one credential this section needs is named indirectly,
through `credential_sources`, which is the whole point of that section.

### The three required fields

| Key | What it is |
|---|---|
| `base_url` | The cluster-internal endpoint. **https only**, and refused if it carries userinfo: this connection carries a bearer token and a run's whole output. |
| `profile` | The operator-approved profile a sandbox is provisioned from. BEN never submits a pod or sandbox spec; it names this and Airlock pins an immutable revision. |
| `auth_source` | A `credential_sources` entry that provides the backend bearer token. |

### Direct workload identity

`projected_oidc` is the production source when Airlock validates the workload's
projected token directly. Mount a bounded ServiceAccount token with Airlock's
configured audience at `token_path`; kubelet may rotate the file in place. BEN
re-reads it for every fetch and accepts it only when the JWT payload carries:

- the exact configured `issuer`;
- `aud` as either that `audience` string or a list containing it;
- `tenant_claim` equal to `tenant_id`;
- `sub` equal to `subject`; and
- an integer expiry at least `min_ttl_ms` in the future.

BEN returns a conservative deadline of `now + min_ttl_ms`, never the token's
whole observed lifetime. A projection inside that floor is a transient refusal:
kubelet rotation can make the next read succeed. An unreadable, oversized,
malformed or identity-mismatched token is permanent. Neither form prints the
token or its raw claims.

The source's durable principal is exactly Airlock's ownership equality,
`(tenant_id, subject)`. `client_id` is intentionally absent: Airlock records it
but excludes it from owner equality, so an OAuth client rotation must not strand
one workload's sandbox. Editing the issuer, audience, claim mapping, tenant,
subject, path or TTL is still a source-definition change and refuses retained
records under the ordinary substrate binding.

BEN parses these claims only to prove that a rotated token still addresses the
persisted replay scope. It does not validate the signature and grants no
authority from the parse. Airlock remains responsible for discovery, signature,
issuer, audience and expiry validation and for every authorization decision. A
forged token with matching text is rejected by Airlock; a valid token for a
different tenant or subject is refused by BEN before it reaches Airlock.

`octo_sts` remains the exchange source for the short-lived GitHub credentials it
mints. `static` may carry an Airlock token in development, but it is opaque and
therefore binds retained work to the full token digest. Changing that token
across a retry or restart deliberately returns `ErrSubstrateBinding`; do not use
it to claim projected-token restart recovery.

### The credential is a fourth identity, and must be

`substrate.airlock.auth_source` may not resolve to the same credential as the
tracker's or the publisher's. A workflow where it does is a **load refusal**.

This is SPEC §10.2's rule applied to a wider blast radius. §10.2 keeps the
tracker credential away from the agent because an agent holding it can rewrite
the queue that dispatched it. A token that can create and destroy execution
environments is a third authority again, and it has no business also being one
scoped to the forge. In `docs/DEPLOY.md` terms: mint it under its own workload
identity, with its own trust policy.

Nothing else changes about BEN's credentials. No BEN tracker or legacy publish
credential is ever serialized into an Airlock request or into a sandbox's
environment, and the backend's own token never appears in a request body — only
in the `Authorization` header. Remote adapters also refuse the standard
`GH_TOKEN`, `GITHUB_TOKEN`,
`GITHUB_API_TOKEN`, `GH_ENTERPRISE_TOKEN`, and `GITHUB_ENTERPRISE_TOKEN`
environment surfaces even when they appear explicitly in `agent.provider.env`:
those names grant reusable GitHub authority, while remote publication belongs
to Airlock's typed publish phase and central broker. The same refusal follows
source provenance across the entire provider block, so renaming `$GH_TOKEN` into an
innocuous environment key or placing it in an argv-bound field such as `model`
does not evade the boundary. The remote structural check applies this rule in
`ben config effective` and at the first stage of daemon assembly, before BEN
constructs credentials, contacts Airlock, or can claim work.

### Retention, and what `delete` means

The four `on_*` keys say what BEN does with a claim's remote workspace when it
publishes, fails, is revoked, or when the daemon shuts down. Shutdown is the one
outcome that does not end the tracker claim: BEN leaves that claim and its cycle
identity standing for startup recovery.

| Keyword | Effect |
|---|---|
| `retain` | Leave the sandbox as it is — warm, allocated, and costing. Mirrors §6.4's local rule that a failed attempt's worktree is kept for forensics. |
| `suspend` | Release compute, keep the persistent volume. A later attempt on the same claim resumes rather than rebuilds. |
| `delete` | Destroy compute **and the volume**. The only disposal that loses work. |

`retain` and `suspend` also retain BEN's workspace-cycle record. That record
carries the original acquisition base, so a controller reassignment under the
same standing approval can resume the same sandbox while minting a fresh
verification base. BEN retires the cycle record only after `delete` actually
lands and the claim has ended. Even `on_shutdown: delete` keeps the daemon-side
cycle and verification pin: the next start must be able to reacquire the still-
claimed cycle deliberately rather than reinterpret it as a new approval.

Three things are not configurable, on purpose:

- **A retry always reuses the workspace.** §6.2 reattaches rather than
  recreating, and the tree carries the previous attempt's work that §9.6's
  continuation prompt reports on.
- **`delete` is never a default.** A policy nobody wrote must not destroy a tree
  somebody may still need.
- **No disposal happens while a run's termination is unconfirmed.** See below.

### `on_revoked` is what ends a *workspace cycle*, not just a claim

A published claim does not release its sandbox. `done` disposes under
`on_success`, which remote review forces to `suspend` so the reviewer can resume
the same tree ([REVIEW.md](REVIEW.md)) — and BEN then *retains* the tracker claim
while the PR awaits review. The sandbox and its volume survive that whole
window, which is the point.

What ends the cycle is the tracker, and it is the same two facts a running
claim's revocation is: the complete required-label set is removed — which is what
the review controller does when it routes a clean result — or the issue goes
terminal, which is what merging a PR carrying `Fixes #<n>` does. BEN applies
`on_revoked` at that instant. A reapproval afterwards mints a *different* cycle
address, so nothing will ever attach to the old sandbox again.

> **With review enabled, `on_revoked: suspend` retains a volume per completed
> issue, indefinitely.** The default is `suspend` because a default must not
> destroy anything; a deployment running review to completion wants
> `on_revoked: delete`, and the profile's `delete_after_idle_ms` is a backstop
> measured in days rather than a policy.

The route that deliberately disposes nothing is a review that requests changes:
the controller unassigns BEN with every required label standing, so the cycle is
still alive and the next claim epoch reuses the same sandbox.

A withdrawal and reapplication while the daemon is down is still observable.
When claim preparation first sees the new approval, it writes an ended-cycle
obligation containing the old cycle's complete backend and verification identity
under the old opaque address, then writes the replacement cycle. The obligation
does not share the replacement's file or lifetime. A crash between those writes
leaves the old cycle live and the obligation owed; disposal refuses until replay
finishes the replacement, so one cycle cannot be both the live record and the
thing being deleted.

Startup reads these obligations before classifying tracker claims, and every
tick retries the local read. An unreadable entry blocks dispatch and tracker-claim
release globally because it cannot say which issue it owns. One issue may owe
several addresses; each is driven independently, and the tracker claim is not
released until all of them confirm. A replacement may prepare or finish while an
old delete is in flight: its live record is never resolved through the old
address.

The daemon-side mirror retains every old assignment pin named by an obligation
when it records the replacement's pin. For `delete`, BEN first persists Airlock's
confirmation, then removes that old pin, then removes the obligation. A crash in
that cleanup window resumes local cleanup without sending the policy operation
again. `retain` and `suspend` keep the obligation record and pin as the durable
address of what the policy deliberately left behind, but no longer report work
to the orchestrator.

Under `on_revoked: delete`, the deletion is ordered **before** the tracker claim
is released, and BEN does not stop owing it until Airlock's three evidence fields
confirm. Live-observed revocations are re-derived from the standing claim after
a restart; downtime replacements additionally have the durable obligation above,
which recovery enumerates before it considers that claim dispatchable.

A refused deletion is retried and reported per attempt, naming the issue and the
cycle. It holds its own claim and never another's: a claim owing no disposal
releases on its own schedule, and the disposal runs off the daemon's effect queue
so it cannot stall an unrelated issue's label writes either. What ended cycles
*do* share is **one new disposal start per tick**, offered in a rotation that
advances on the offer — so one whose control plane is slow or refusing cannot
hold later cycles behind it. That is §9.8's decision 14 applied to a backend
call: a backlog-wide label clear ends every cycle in the backlog at once, and
without the bound it would be that many concurrent deletions, then that many
again on every tick for as long as any of them kept failing. Retries are paced by
the poll tick; calls already in flight are skipped so later addresses receive the
next turns.

The delete itself is additionally gated on the *sandbox*, and on two facts rather
than one. `DELETE` is the one workspace verb Airlock does not refuse over a live
run — suspend answers `run_conflict`, delete moves the sandbox to `deleting` and
marks whatever was executing in it `lost` — and a review executes in the same
sandbox under its own run id, which BEN's claim journal cannot see. So BEN reads
the sandbox first and refuses unless:

- **no run holds the active slot**, and
- **the sandbox is not `failed`**.

The second is not redundant. Airlock releases the active slot when a run reaches
*any* terminal state, and moves the sandbox `ready → failed` in the same step when
that run's domain quiet was not confirmed. An empty slot therefore says every run
terminated; it does not say the domain is quiet, and `failed` is exactly where a
run whose quiet nobody could attest leaves the sandbox. Deleting there would
destroy a volume a process may still be writing to.

A sandbox that ends up `failed` is consequently one BEN will not delete. That is
deliberate and it is the same asymmetry §9.8 takes everywhere else — a refusal
costs a retained allocation and another tick, the other answer costs the volume —
but it means the profile's `delete_after_idle_ms` and an operator are the backstop
for that case rather than `on_revoked`.

`idle_suspend_ms` and `delete_after_idle_ms` ask *Airlock* to enforce the same
two windows on its own, clamped to the profile's maxima. They exist because a
daemon that crashes between a claim's attempts would otherwise leave a warm
sandbox and a volume allocated with nothing left to release them. Both have a
60-second floor, which is the contract's; a shorter value is refused rather than
silently raised.

### The substrate cannot be changed by reload

Editing `substrate:` — or the credential-source definition its `auth_source`
names — under a running daemon is refused, exactly as editing `deployment:` is,
and the last-known-good declaration stays in force while dispatch blocks. The
reason is specific to this section: outstanding claims hold sandbox ids, run
bindings and event cursors addressed against the backend and principal they were
dispatched to. Moving either under them would strand every one of those while a
live agent kept running somewhere BEN had stopped looking.

A restart adopts it for new work. Retained records remain bound to the canonical
endpoint and non-secret credential-source identity that wrote them. Every keyed
request snapshots one exact credential for all of its internal retries. Before
a sandbox or run create, BEN also persists a runtime principal binding from
that snapshot. `octo_sts` fixes its principal in the trust policy;
`projected_oidc` verifies its configured tenant and subject in every rotated
token. An opaque `static` source instead binds the full SHA-256 of the token
value, so replacing the same `$VAR` with another principal's token refuses
ambiguous replay. Signals need no third
durable address: each call compares its snapshot with the run's persisted
principal binding, then retains that snapshot for its whole retry lifetime. The
token itself is never persisted. An outstanding claim refuses rather than
replaying or attaching across either boundary.

## What BEN persists, and why

Two record kinds, under `<state dir>/substrate/` (SPEC §10.3):

- **The sandbox record** — the canonical endpoint, non-secret credential-source
  and runtime principal bindings, the create idempotency key and its local
  replay fence, then which sandbox this claim cycle acquired, at which immutable
  profile revision, and under which owning principal. The bindings, key and
  fence are written *before* creation is attempted; Airlock's answers are added
  before anything runs in the sandbox. Since #284 it also carries the stdin
  envelope of the pinned revision, recorded the first time a profile read
  reports that exact revision, because that is the only moment it is readable
  and the only bound a run in this sandbox is actually judged by.
- **The run binding** — the local replay fence written before an agent,
  lifecycle hook, Git prepare, or Git publish start is attempted, the same
  substrate and runtime principal bindings, then which Airlock run id that
  start resolved to. Two more facts land here when they apply (#284): that a
  streaming run's stdin is still owed, written before the start that creates
  the run and cleared after the close is receipted; and a definite refusal of
  the start — the code, the sanitized message, the exceeded limit, and a
  fingerprint of the exact body Airlock refused — so an unchanged body is
  answered from the record and only a changed one is sent.

Neither is authority. The tracker and git remain the source of truth (SPEC
§9.10); these are *addresses*, and the reason a wrong one is worse than a missing
one is that a missing record parks a claim while a wrong one dispatches into
somebody else's sandbox.

They exist because Airlock deliberately does not answer the questions they
answer. There is no list-sandboxes-by-label route — so a daemon with no record
could only re-derive a sandbox by *creating* one, which is exactly what
reattaching must never do. And a run is addressed by Airlock's own durable
handle, while the idempotency key that produced it expires after 24 hours.

Every idempotency key BEN sends is **derived**, not generated: from the claim
cycle, the branch, the trusted base and the profile for a sandbox; from the
sandbox, BEN's run id and a canonical digest of the immutable request for a run.
Airlock scopes those keys by tenant and subject as well as route, so every
record compares its endpoint and credential-source binding before any replay or
resource access. That tuple contains no bearer token.

Once a sandbox id is recorded, every acquire addresses it with GET and never
replays creation. An unanswered creation may be replayed only inside Airlock's
24-hour idempotency window; after that, or for an older record with no durable
fence, BEN parks the claim rather than risk allocating a second sandbox. Agent,
hook, prepare, and publish starts follow the same rule: once a run id is recorded
BEN addresses it with GET; an ambiguous start is replayed only before its
persisted 24-hour fence expires. Prepare, coding, publish, and review each have a
distinct durable run identity. Each phase must reach terminal plus domain quiet
before the next begins, and publish retries reuse one derived operation key so
Airlock can converge on its durable receipt.

For agent runs the two local records make three superficially similar states
distinguishable. No Airlock run-binding fence means the request never reached a
backend start and is safe to retire. A fence with no run id is an unanswered
start. The pre-tracker startup survey checks the sandbox's single active slot
and adopts it only when the run's `ben.process` label names the exact journaled
request. With no active run it reports `start_unresolved`; it does not replay,
because the approval may have been revoked while BEN was down. Recovery first
passes §9.10's tracker and pinned-epoch gates and reads §9.7 publish evidence.
Published work and a verifier error never replay; only an incomplete or
contradicted active projection that the classifier routes to orphan backoff may
continue. Its standing required-label event must then match both the persisted
workspace cycle and a fresh tracker read. Only then may BEN reconstruct the
exact request from the journal's non-secret `RunSpec`, recompose provider
configuration and credentials in memory, verify the full request digest, and
replay the original key. Revoked, terminal, completed, and superseded-approval
work is only observed and can never take that path. A binding with a run id
followed by `404` means the accepted resource is unavailable, not absent. That
state remains an operator-visible reconciliation error and never authorizes
workspace reuse.

### How a prompt reaches a run

The prompt travels on stdin, and the profile bounds stdin three ways: inline in
the start request up to `max_stdin_inline_bytes`, or streamed to the run
afterwards in writes of at most `max_stdin_chunk_bytes`, both under
`max_stdin_total_bytes`; and over every request sits `max_request_body_bytes`,
measured on the encoded body before it is parsed — an inline prompt is base64
inside JSON, so a body can exceed that while the decoded prompt is under the
inline bound, and each streaming write is a body of its own. BEN chooses per
prompt: inline while both the decoded prompt and the encoded body fit,
streaming above that, in chunks cut so each write's body fits too. Which path
a prompt takes is a function of its length and the sandbox's envelope and of
nothing else, so the same address composes the same start body on every
replay, which is what the idempotency key requires.

**The envelope is the sandbox's, not the profile's.** A sandbox is pinned to
the profile revision it was created against and stays there while the profile
rolls forward, and its runs are judged by the pin. Airlock exposes a profile
only at its current revision, so the pinned envelope is readable exactly while
the two agree — which is every acquire until a rollout — and BEN records it on
the sandbox record the first time a read matches (`limits`). It is retried at
every acquire, attach and start until it is known. A sandbox whose envelope
BEN never read, because the profile had already moved when the daemon first
looked, is planned the way every prompt was before #284: inline, with the
backend as the judge and its refusal a definite, surfaced answer. Reading the
current profile at readiness serves only the assembly's configuration check
(`review.max_diff_bytes` against `max_stdin_total_bytes`); dispatch never
plans against it.

Streaming is offset-addressed and receipted by the contract. BEN writes from
offset zero and closes with the last chunk; an exact resend at a receipted
offset is a no-op that returns the recorded `next_offset`, so a daemon that dies
mid-delivery resumes on its next start at the address by walking the delivered
prefix and appending the rest. The run binding records that the delivery is
owed until the close is receipted. A run that is already terminal, or whose
compute generation is gone, ends the delivery without completing it; the run's
own evidence decides what happens next.

A prompt over the total bound has no path, and BEN answers that as the refusal
Airlock would return, without the round trip. It is the same recorded fact: the
next section's `refused to admit the process`.

## What it refuses, and what that means for you

Each of these parks the claim rather than retrying, and each names a thing you
can act on.

| Refusal | What happened |
|---|---|
| `durable state belongs to a different substrate` | The record has no substrate binding, or names another canonical endpoint or credential-source identity. Replaying its key could create a second resource in another tenant. BEN retains the record and sends no request; reconcile it against the endpoint and credential that wrote it. |
| `sandbox profile revision does not match the pinned revision` | The profile moved under a live claim, or an operator withdrew the revision it pinned. A sandbox id matching while its world changed is the hazard the pin exists to close. Delete the claim's workspace and let it re-acquire. |
| `this principal does not own the recorded sandbox` | The record names a sandbox in another tenant, or owned by another subject. Airlock answers `404` across tenants and `403` within one; both mean the same thing here. OAuth client id is audit metadata, not ownership, so rotating it does not cause this refusal. Never repaired automatically — removing the record would silently release a workspace that may be somebody else's. |
| `the sandbox create replay window has expired` | Creation may have succeeded without returning its sandbox id, but the keyed result is now too old to replay safely. Reconcile the recorded idempotency key in Airlock; BEN retains the record and will not create a replacement. |
| `the run start replay window has expired` | An agent or hook start may have succeeded without returning its run id, but the keyed result is now too old to replay safely. Reconcile the recorded idempotency key in Airlock; BEN retains the binding and will not start a replacement. |
| `process start outcome is unresolved` | The start fence landed but no permanent run id was learned. Startup retains the claim without replay; exact replay requires an orphan/backoff §9.7 verdict and the same still-standing approval cycle. Revoked, completed, unverifiable, or reapproved work remains unresolved rather than being launched. |
| `backend refused to admit the process; nothing was started` | Airlock's request validation refused the start body — `413 payload_too_large`, `422 env_rejected`, `400 invalid_request` — before its idempotency claim, so nothing exists and nothing is stored under the key (#284). Definite: every later read of the address says so, and it also reads as a confirmed absence, so the coding path fails the attempt as a launch that never happened and the review path records it and states it on the issue. BEN re-sends only a *different* body under the address; an unchanged one is answered from the binding. Act on the code and limit it names: raise the profile's bound, or lower what BEN composes (`review.max_diff_bytes`, the prompt template). |
| `accepted process is unavailable` | Airlock previously returned a permanent run id, but that resource now answers as unavailable. A tombstone and cross-tenant hiding can look identical, so BEN retains it as unconfirmed rather than treating `404` as termination. |
| `run termination is unconfirmed; the workspace may not be touched` | A disposal was requested over a run whose execution domain has not been observed quiet. |
| `backend event sequence has a gap` | Evidence in the event log BEN cannot admit — an unexplained jump, a durable envelope whose shape, output encoding or stream is unusable, or a `cursor_too_old` that names neither the cursor it is about nor the oldest sequence still retained. Re-reading an append-only malformed envelope cannot repair it; advancing over any of these would commit BEN to events it never saw *and* cannot name. The measured expiry is not this refusal; see below. |
| `backend replayed a sequence with a different payload` | The backend's log is not the log BEN read. No dedupe rule can repair that. |
| `sandbox deletion is not confirmed` | `deleteSandbox` returned, and compute release, volume destruction and record tombstoning have not all confirmed. The record is deliberately kept: a forgotten record is a sandbox nothing will ever finish deleting. |

### The one gap BEN does not park on

Airlock's event log has a retention window, and a slow or interrupted daemon can
come back to find its cursor below it. That is data loss, and it is also the one
discontinuity BEN advances over — because a claim parked forever on a retention
policy is not a better answer, and it is not one a daemon can recover from on
its own.

What makes the advance safe is that BEN can say exactly what it lost. A
`cursor_too_old` carrying both `requested_after` — which must be the cursor that
call actually sent — and `oldest_available_seq` describes a specific range of
sequences. Anything less stays the refusal in the table above: an expiry stated
about somebody else's cursor, or with either number missing or not a sequence, is
not evidence about this request.

When BEN can measure it, one durable act records all of it:

- the range `requested_after + 1 .. oldest_available_seq - 1`, written into the
  durable event history as an **accepted** loss;
- the cursor, advanced to `oldest_available_seq - 1`;
- any partial provider line held by the decoder, dropped — bytes on the far side
  of a hole cannot complete a record from this side;
- exactly one `failed(crashed)` outcome for the attempt.

The failure is the acceptance, not a consequence of it. Provider output BEN never
read cannot be re-read, so an incomplete stream may never become success: BEN
keeps draining what Airlock still holds and keeps copying it to the transcript,
and termination evidence is still observed, but nothing past the gap is
translated into a second outcome. A retained `success` line on the far side of an
expiry is retained *as evidence* and never as a verdict.

The same rule means a gap first discovered after BEN has already made `succeeded`
durable is not accepted. SPEC §7.4 makes that terminal provider event ground
truth; replacing it with failure would violate the outcome contract, while
advancing without the failure would make the range mean something different.
BEN refuses that expiry without moving the cursor. A later gap after an already
accepted failure remains admissible and carries no second outcome.

A log that expired entirely — `oldest_available_seq == latest_seq + 1` — is the
same rule with nothing left over: the range runs through the known end and the
cursor advances to it, so the very next poll is answered rather than expiring
again.

The recorded range is what a restart reads. It is the only thing that makes a
sequence missing from BEN's durable history an accepted discontinuity rather than
evidence that history is corrupt, which is why the gap is never silently skipped
and the transcript it leaves is never presented as complete.

## The rule underneath all of it

**A signal is not a death, a stream ending is not a reap, and a reap is not a
quiet execution domain.**

Airlock reports those as three independent evidence fields that start at
`unknown` and only positive observation moves. BEN maps them one-to-one onto the
three facts #192 already distinguishes, and derives nothing across them. In
particular:

- A `202` from `signalRun` acknowledges the *request*. It says nothing about
  delivery, and delivery says nothing about termination.
- A run in `lost` — one Airlock can no longer obtain evidence about — reports
  domain quiet `unknown` forever. It is never readable as termination.
- An unreachable control plane is the *absence* of an answer. It reports
  unconfirmed, and a reconciliation that could not reach the backend keeps
  costing its lease rather than freeing capacity. The event stream is the same
  rule on the read side: `follow` is cursor-addressed and retained, so a read
  that failed at the transport is BEN failing to reach an untouched run. The
  attempt reconnects from its admitted cursor and, past the reconnect budget, is
  *held* rather than failed — an API pod that goes away mid-run takes the read
  path with it and nothing else
  ([#275](https://github.com/srhg-ai-7cef3f93/ben/issues/275)). The client's own
  per-request retries are the short end of the same problem: they absorb a blip,
  the reconnect absorbs a rollout.

Only an explicit domain-quiet observation from a reachable backend authorizes
suspending, deleting, or dispatching a replacement run into a workspace.

And, separately: **even a successful trusted publish response is not BEN's
evidence of a pushed branch or an opened pull request.** Publication is verified
daemon-side from git facts no run can reach
([#193](https://github.com/srhg-ai-7cef3f93/ben/issues/193), SPEC §9.7).

## Concurrency

`limits.max_concurrent_agents` counts a different thing on this substrate, and it
is the same rule applied to the scarce resource.

Locally it counts agent processes on the daemon's host. Remotely the scarce thing
is the backend's allocation, so it counts **active backend runs and leases**:

- A held lease costs even with no run in it. A suspended workspace is still a
  reservation, and a daemon that counted only live runs could hold every sandbox
  its quota allows while reporting itself idle.
- A dispatched run costs until its termination is *confirmed*, not until its
  stream ends.

## Testing it

`make check` covers this backend end to end with no network: `internal/airlock`
runs against `internal/airlock/airlocktest`, an in-process server faithful to the
frozen contract — keyed idempotency with payload fingerprints, the single active
run slot, three independent evidence fields, contiguous per-run event sequences,
ownership checked before state, and confirmed deletion. Lost responses, daemon
restarts, replay, gaps, conflicts, cross-tenant collisions, network partitions
and profile-revision mismatches are all scripted there — including the #275
shape, where the control plane goes away mid-run and the run's own `exit 0` is
still what BEN reports.

For a real endpoint there is `scripts/airlock-smoke.sh`: one fixture command in
one sandbox, end to end, against credentials you supply. It is **opt-in and
credential-gated** and is not part of `make check`, for the reason
[docs/SMOKE.md](SMOKE.md) gives about the §12.4 smoke — CI stays offline and
deterministic.

It reports success only on an explicit pass for the test it names (#244). A
`go test -run` pattern that matches nothing exits 0, and so does the test
skipping itself over an unset variable, so the wrapper anchors the pattern and
reads the result out of the run's own output; `internal/arch` pins the test name
and the three variables to `internal/airlock/smoke_test.go` and drives the
verdict rule over recorded output, so both refusals are checked by `make check`
without a credential.
